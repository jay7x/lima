package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lima-vm/lima/pkg/driver"
	"github.com/lima-vm/lima/pkg/driverutil"
	"github.com/lima-vm/lima/pkg/networks"

	"github.com/lima-vm/lima/pkg/cidata"
	guestagentapi "github.com/lima-vm/lima/pkg/guestagent/api"
	guestagentclient "github.com/lima-vm/lima/pkg/guestagent/api/client"
	hostagentapi "github.com/lima-vm/lima/pkg/hostagent/api"
	"github.com/lima-vm/lima/pkg/hostagent/dns"
	"github.com/lima-vm/lima/pkg/hostagent/events"
	"github.com/lima-vm/lima/pkg/limayaml"
	"github.com/lima-vm/lima/pkg/sshutil"
	"github.com/lima-vm/lima/pkg/store"
	"github.com/lima-vm/lima/pkg/store/filenames"
	"github.com/lima-vm/sshocker/pkg/ssh"
	"github.com/sethvargo/go-password/password"
	"github.com/sirupsen/logrus"
)

type HostAgent struct {
	y               *limayaml.LimaYAML
	sshLocalPort    int
	udpDNSLocalPort int
	tcpDNSLocalPort int
	instDir         string
	instName        string
	instSSHAddress  string
	sshConfig       *ssh.SSHConfig
	portForwarder   *portForwarder
	onClose         []func() error // LIFO
	guestAgentProto guestagentclient.Proto

	driver   driver.Driver
	sigintCh chan os.Signal

	eventEnc   *json.Encoder
	eventEncMu sync.Mutex

	vSockPort int
}

type options struct {
	nerdctlArchive string // local path, not URL
}

type Opt func(*options) error

func WithNerdctlArchive(s string) Opt {
	return func(o *options) error {
		o.nerdctlArchive = s
		return nil
	}
}

// New creates the HostAgent.
//
// stdout is for emitting JSON lines of Events.
func New(instName string, stdout io.Writer, sigintCh chan os.Signal, opts ...Opt) (*HostAgent, error) {
	var o options
	for _, f := range opts {
		if err := f(&o); err != nil {
			return nil, err
		}
	}
	inst, err := store.Inspect(instName)
	if err != nil {
		return nil, err
	}

	y, err := inst.LoadYAML()
	if err != nil {
		return nil, err
	}
	// y is loaded with FillDefault() already, so no need to care about nil pointers.

	sshLocalPort, err := determineSSHLocalPort(y, instName)
	if err != nil {
		return nil, err
	}
	if *y.VMType == limayaml.WSL2 {
		sshLocalPort = inst.SSHLocalPort
	}

	var udpDNSLocalPort, tcpDNSLocalPort int
	if *y.HostResolver.Enabled {
		udpDNSLocalPort, err = findFreeUDPLocalPort()
		if err != nil {
			return nil, err
		}
		tcpDNSLocalPort, err = findFreeTCPLocalPort()
		if err != nil {
			return nil, err
		}
	}

	guestAgentProto := guestagentclient.UNIX
	if *y.VMType == limayaml.WSL2 {
		guestAgentProto = guestagentclient.VSOCK
	}

	vSockPort := 0
	if guestAgentProto == guestagentclient.VSOCK {
		port, err := getFreeVSockPort()
		if err != nil {
			logrus.WithError(err).Error("failed to get free VSock port")
		}
		vSockPort = port
	}

	if err := cidata.GenerateISO9660(inst.Dir, instName, y, udpDNSLocalPort, tcpDNSLocalPort, o.nerdctlArchive, vSockPort); err != nil {
		return nil, err
	}

	sshOpts, err := sshutil.SSHOpts(inst.Dir, *y.SSH.LoadDotSSHPubKeys, *y.SSH.ForwardAgent, *y.SSH.ForwardX11, *y.SSH.ForwardX11Trusted)
	if err != nil {
		return nil, err
	}
	if err = writeSSHConfigFile(inst, inst.SSHAddress, sshLocalPort, sshOpts); err != nil {
		return nil, err
	}
	sshConfig := &ssh.SSHConfig{
		AdditionalArgs: sshutil.SSHArgsFromOpts(sshOpts),
	}

	rules := make([]limayaml.PortForward, 0, 3+len(y.PortForwards))
	// Block ports 22 and sshLocalPort on all IPs
	for _, port := range []int{sshGuestPort, sshLocalPort} {
		rule := limayaml.PortForward{GuestIP: net.IPv4zero, GuestPort: port, Ignore: true}
		limayaml.FillPortForwardDefaults(&rule, inst.Dir)
		rules = append(rules, rule)
	}
	rules = append(rules, y.PortForwards...)
	// Default forwards for all non-privileged ports from "127.0.0.1" and "::1"
	rule := limayaml.PortForward{GuestIP: guestagentapi.IPv4loopback1}
	limayaml.FillPortForwardDefaults(&rule, inst.Dir)
	rules = append(rules, rule)

	limaDriver := driverutil.CreateTargetDriverInstance(&driver.BaseDriver{
		Instance:     inst,
		Yaml:         y,
		SSHLocalPort: sshLocalPort,
	})

	a := &HostAgent{
		y:               y,
		sshLocalPort:    sshLocalPort,
		udpDNSLocalPort: udpDNSLocalPort,
		tcpDNSLocalPort: tcpDNSLocalPort,
		instDir:         inst.Dir,
		instName:        instName,
		instSSHAddress:  inst.SSHAddress,
		sshConfig:       sshConfig,
		portForwarder:   newPortForwarder(sshConfig, sshLocalPort, rules, inst.VMType),
		driver:          limaDriver,
		sigintCh:        sigintCh,
		eventEnc:        json.NewEncoder(stdout),
		vSockPort:       vSockPort,
		guestAgentProto: guestAgentProto,
	}
	return a, nil
}

func writeSSHConfigFile(inst *store.Instance, instSSHAddress string, sshLocalPort int, sshOpts []string) error {
	if inst.Dir == "" {
		return fmt.Errorf("directory is unknown for the instance %q", inst.Name)
	}
	var b bytes.Buffer
	if _, err := fmt.Fprintf(&b, `# This SSH config file can be passed to 'ssh -F'.
# This file is created by Lima, but not used by Lima itself currently.
# Modifications to this file will be lost on restarting the Lima instance.
`); err != nil {
		return err
	}
	if err := sshutil.Format(&b, inst.Name, sshutil.FormatConfig,
		append(sshOpts,
			fmt.Sprintf("Hostname=%s", instSSHAddress),
			fmt.Sprintf("Port=%d", sshLocalPort),
		)); err != nil {
		return err
	}
	fileName := filepath.Join(inst.Dir, filenames.SSHConfig)
	return os.WriteFile(fileName, b.Bytes(), 0o600)
}

func determineSSHLocalPort(y *limayaml.LimaYAML, instName string) (int, error) {
	if *y.SSH.LocalPort > 0 {
		return *y.SSH.LocalPort, nil
	}
	if *y.SSH.LocalPort < 0 {
		return 0, fmt.Errorf("invalid ssh local port %d", y.SSH.LocalPort)
	}
	switch instName {
	case "default":
		// use hard-coded value for "default" instance, for backward compatibility
		return 60022, nil
	default:
		sshLocalPort, err := findFreeTCPLocalPort()
		if err != nil {
			return 0, fmt.Errorf("failed to find a free port, try setting `ssh.localPort` manually: %w", err)
		}
		return sshLocalPort, nil
	}
}

func findFreeTCPLocalPort() (int, error) {
	lAddr0, err := net.ResolveTCPAddr("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp4", lAddr0)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	lAddr := l.Addr()
	lTCPAddr, ok := lAddr.(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("expected *net.TCPAddr, got %v", lAddr)
	}
	port := lTCPAddr.Port
	if port <= 0 {
		return 0, fmt.Errorf("unexpected port %d", port)
	}
	return port, nil
}

func findFreeUDPLocalPort() (int, error) {
	lAddr0, err := net.ResolveUDPAddr("udp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenUDP("udp4", lAddr0)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	lAddr := l.LocalAddr()
	lUDPAddr, ok := lAddr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("expected *net.UDPAddr, got %v", lAddr)
	}
	port := lUDPAddr.Port
	if port <= 0 {
		return 0, fmt.Errorf("unexpected port %d", port)
	}
	return port, nil
}

func (a *HostAgent) emitEvent(_ context.Context, ev events.Event) {
	a.eventEncMu.Lock()
	defer a.eventEncMu.Unlock()
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if err := a.eventEnc.Encode(ev); err != nil {
		logrus.WithField("event", ev).WithError(err).Error("failed to emit an event")
	}
}

func generatePassword(length int) (string, error) {
	// avoid any special symbols, to make it easier to copy/paste
	return password.Generate(length, length/4, 0, false, false)
}

func (a *HostAgent) Run(ctx context.Context) error {
	defer func() {
		exitingEv := events.Event{
			Status: events.Status{
				Exiting: true,
			},
		}
		a.emitEvent(ctx, exitingEv)
	}()

	firstUsernetIndex := limayaml.FirstUsernetIndex(a.y)
	if firstUsernetIndex == -1 && *a.y.HostResolver.Enabled {
		hosts := a.y.HostResolver.Hosts
		hosts["host.lima.internal"] = networks.SlirpGateway
		hosts[fmt.Sprintf("lima-%s", a.instName)] = networks.SlirpIPAddress
		srvOpts := dns.ServerOptions{
			UDPPort: a.udpDNSLocalPort,
			TCPPort: a.tcpDNSLocalPort,
			Address: "127.0.0.1",
			HandlerOptions: dns.HandlerOptions{
				IPv6:        *a.y.HostResolver.IPv6,
				StaticHosts: hosts,
			},
		}
		dnsServer, err := dns.Start(srvOpts)
		if err != nil {
			return fmt.Errorf("cannot start DNS server: %w", err)
		}
		defer dnsServer.Shutdown()
	}

	errCh, err := a.driver.Start(ctx)
	if err != nil {
		return err
	}

	// WSL instance SSH address isn't known until after VM start
	if *a.y.VMType == limayaml.WSL2 {
		sshAddr, err := store.GetSSHAddress(a.instName)
		if err != nil {
			return err
		}
		a.instSSHAddress = sshAddr
	}

	if a.y.Video.Display != nil && *a.y.Video.Display == "vnc" {
		vncdisplay, vncoptions, _ := strings.Cut(*a.y.Video.VNC.Display, ",")
		vnchost, vncnum, err := net.SplitHostPort(vncdisplay)
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(vncnum)
		if err != nil {
			return err
		}
		vncport := strconv.Itoa(5900 + n)
		vncpwdfile := filepath.Join(a.instDir, filenames.VNCPasswordFile)
		vncpasswd, err := generatePassword(8)
		if err != nil {
			return err
		}
		if err := a.driver.ChangeDisplayPassword(ctx, vncpasswd); err != nil {
			return err
		}
		if err := os.WriteFile(vncpwdfile, []byte(vncpasswd), 0o600); err != nil {
			return err
		}
		if strings.Contains(vncoptions, "to=") {
			vncport, err = a.driver.GetDisplayConnection(ctx)
			if err != nil {
				return err
			}
			p, err := strconv.Atoi(vncport)
			if err != nil {
				return err
			}
			vncnum = strconv.Itoa(p - 5900)
			vncdisplay = net.JoinHostPort(vnchost, vncnum)
		}
		vncfile := filepath.Join(a.instDir, filenames.VNCDisplayFile)
		if err := os.WriteFile(vncfile, []byte(vncdisplay), 0o600); err != nil {
			return err
		}
		vncurl := "vnc://" + net.JoinHostPort(vnchost, vncport)
		logrus.Infof("VNC server running at %s <%s>", vncdisplay, vncurl)
		logrus.Infof("VNC Display: `%s`", vncfile)
		logrus.Infof("VNC Password: `%s`", vncpwdfile)
	}

	if a.driver.CanRunGUI() {
		go func() {
			err = a.startRoutinesAndWait(ctx, errCh)
			if err != nil {
				logrus.Error(err)
			}
		}()
		return a.driver.RunGUI()
	}
	return a.startRoutinesAndWait(ctx, errCh)
}

func (a *HostAgent) startRoutinesAndWait(ctx context.Context, errCh chan error) error {
	stBase := events.Status{
		SSHLocalPort: a.sshLocalPort,
	}
	stBooting := stBase
	a.emitEvent(ctx, events.Event{Status: stBooting})
	ctxHA, cancelHA := context.WithCancel(ctx)
	go func() {
		stRunning := stBase
		if haErr := a.startHostAgentRoutines(ctxHA); haErr != nil {
			stRunning.Degraded = true
			stRunning.Errors = append(stRunning.Errors, haErr.Error())
		}
		stRunning.Running = true
		a.emitEvent(ctx, events.Event{Status: stRunning})
	}()
	for {
		select {
		case driverErr := <-errCh:
			logrus.Infof("Driver stopped due to error: %q", driverErr)
			cancelHA()
			if closeErr := a.close(); closeErr != nil {
				logrus.WithError(closeErr).Warn("an error during shutting down the host agent")
			}
			err := a.driver.Stop(ctx)
			return err
		case <-a.sigintCh:
			logrus.Info("Received SIGINT, shutting down the host agent")
			cancelHA()
			if closeErr := a.close(); closeErr != nil {
				logrus.WithError(closeErr).Warn("an error during shutting down the host agent")
			}
			err := a.driver.Stop(ctx)
			return err
		}
	}
}

func (a *HostAgent) Info(_ context.Context) (*hostagentapi.Info, error) {
	info := &hostagentapi.Info{
		SSHLocalPort: a.sshLocalPort,
	}
	return info, nil
}

func (a *HostAgent) startHostAgentRoutines(ctx context.Context) error {
	if *a.y.Plain {
		logrus.Info("Running in plain mode. Mounts, port forwarding, containerd, etc. will be ignored. Guest agent will not be running.")
	}
	a.onClose = append(a.onClose, func() error {
		logrus.Debugf("shutting down the SSH master")
		if exitMasterErr := ssh.ExitMaster(a.instSSHAddress, a.sshLocalPort, a.sshConfig); exitMasterErr != nil {
			logrus.WithError(exitMasterErr).Warn("failed to exit SSH master")
		}
		return nil
	})
	var errs []error
	if err := a.waitForRequirements("essential", a.essentialRequirements()); err != nil {
		errs = append(errs, err)
	}
	if *a.y.SSH.ForwardAgent {
		faScript := `#!/bin/bash
set -eux -o pipefail
sudo mkdir -p -m 700 /run/host-services
sudo ln -sf "${SSH_AUTH_SOCK}" /run/host-services/ssh-auth.sock
sudo chown -R "${USER}" /run/host-services`
		faDesc := "linking ssh auth socket to static location /run/host-services/ssh-auth.sock"
		stdout, stderr, err := ssh.ExecuteScript(a.instSSHAddress, a.sshLocalPort, a.sshConfig, faScript, faDesc)
		logrus.Debugf("stdout=%q, stderr=%q, err=%v", stdout, stderr, err)
		if err != nil {
			errs = append(errs, fmt.Errorf("stdout=%q, stderr=%q: %w", stdout, stderr, err))
		}
	}
	if *a.y.MountType == limayaml.REVSSHFS && !*a.y.Plain {
		mounts, err := a.setupMounts()
		if err != nil {
			errs = append(errs, err)
		}
		a.onClose = append(a.onClose, func() error {
			var unmountErrs []error
			for _, m := range mounts {
				if unmountErr := m.close(); unmountErr != nil {
					unmountErrs = append(unmountErrs, unmountErr)
				}
			}
			return errors.Join(unmountErrs...)
		})
	}
	if len(a.y.AdditionalDisks) > 0 {
		a.onClose = append(a.onClose, func() error {
			var unlockErrs []error
			for _, d := range a.y.AdditionalDisks {
				disk, inspectErr := store.InspectDisk(d.Name)
				if inspectErr != nil {
					unlockErrs = append(unlockErrs, inspectErr)
					continue
				}
				logrus.Infof("Unmounting disk %q", disk.Name)
				if unlockErr := disk.Unlock(); unlockErr != nil {
					unlockErrs = append(unlockErrs, unlockErr)
				}
			}
			return errors.Join(unlockErrs...)
		})
	}
	if !*a.y.Plain {
		go a.watchGuestAgentEvents(ctx)
	}
	if err := a.waitForRequirements("optional", a.optionalRequirements()); err != nil {
		errs = append(errs, err)
	}
	if err := a.waitForRequirements("final", a.finalRequirements()); err != nil {
		errs = append(errs, err)
	}
	// Copy all config files _after_ the requirements are done
	for _, rule := range a.y.CopyToHost {
		if err := copyToHost(ctx, a.sshConfig, a.sshLocalPort, rule.HostFile, rule.GuestFile); err != nil {
			errs = append(errs, err)
		}
	}
	a.onClose = append(a.onClose, func() error {
		var rmErrs []error
		for _, rule := range a.y.CopyToHost {
			if rule.DeleteOnStop {
				logrus.Infof("Deleting %s", rule.HostFile)
				if err := os.RemoveAll(rule.HostFile); err != nil {
					rmErrs = append(rmErrs, err)
				}
			}
		}
		return errors.Join(rmErrs...)
	})
	return errors.Join(errs...)
}

func (a *HostAgent) close() error {
	logrus.Infof("Shutting down the host agent")
	var errs []error
	for i := len(a.onClose) - 1; i >= 0; i-- {
		f := a.onClose[i]
		if err := f(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *HostAgent) watchGuestAgentEvents(ctx context.Context) {
	// TODO: use vSock (when QEMU for macOS gets support for vSock)

	// Setup all socket forwards and defer their teardown
	if *a.y.VMType != limayaml.WSL2 {
		logrus.Debugf("Forwarding unix sockets")
		for _, rule := range a.y.PortForwards {
			if rule.GuestSocket != "" {
				local := hostAddress(rule, guestagentapi.IPPort{})
				_ = forwardSSH(ctx, a.sshConfig, a.sshLocalPort, local, rule.GuestSocket, verbForward, rule.Reverse)
			}
		}
	}

	localUnix := filepath.Join(a.instDir, filenames.GuestAgentSock)
	remoteUnix := "/run/lima-guestagent.sock"

	a.onClose = append(a.onClose, func() error {
		logrus.Debugf("Stop forwarding unix sockets")
		var errs []error
		for _, rule := range a.y.PortForwards {
			if rule.GuestSocket != "" {
				local := hostAddress(rule, guestagentapi.IPPort{})
				// using ctx.Background() because ctx has already been cancelled
				if err := forwardSSH(context.Background(), a.sshConfig, a.sshLocalPort, local, rule.GuestSocket, verbCancel, rule.Reverse); err != nil {
					errs = append(errs, err)
				}
			}
		}
		if err := forwardSSH(context.Background(), a.sshConfig, a.sshLocalPort, localUnix, remoteUnix, verbCancel, false); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	})

	guestSocketAddr := localUnix
	if a.guestAgentProto == guestagentclient.VSOCK {
		guestSocketAddr = fmt.Sprintf("0.0.0.0:%d", a.vSockPort)
	}

	for {
		if !isGuestAgentSocketAccessible(ctx, guestSocketAddr, a.guestAgentProto, a.instName) {
			if a.guestAgentProto != guestagentclient.VSOCK {
				_ = forwardSSH(ctx, a.sshConfig, a.sshLocalPort, localUnix, remoteUnix, verbForward, false)
			}
		}
		if err := a.processGuestAgentEvents(ctx, guestSocketAddr, a.guestAgentProto, a.instName); err != nil {
			if !errors.Is(err, context.Canceled) {
				logrus.WithError(err).Warn("connection to the guest agent was closed unexpectedly")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func isGuestAgentSocketAccessible(ctx context.Context, localUnix string, proto guestagentclient.Proto, instanceName string) bool {
	client, err := guestagentclient.NewGuestAgentClient(localUnix, proto, instanceName)
	if err != nil {
		return false
	}
	_, err = client.Info(ctx)
	return err == nil
}

func (a *HostAgent) processGuestAgentEvents(ctx context.Context, localUnix string, proto guestagentclient.Proto, instanceName string) error {
	client, err := guestagentclient.NewGuestAgentClient(localUnix, proto, instanceName)
	if err != nil {
		return err
	}

	info, err := client.Info(ctx)
	if err != nil {
		return err
	}

	logrus.Debugf("guest agent info: %+v", info)

	onEvent := func(ev guestagentapi.Event) {
		logrus.Debugf("guest agent event: %+v", ev)
		for _, f := range ev.Errors {
			logrus.Warnf("received error from the guest: %q", f)
		}
		a.portForwarder.OnEvent(ctx, ev, a.instSSHAddress)
	}

	if err := client.Events(ctx, onEvent); err != nil {
		return err
	}
	return io.EOF
}

const (
	verbForward = "forward"
	verbCancel  = "cancel"
)

func executeSSH(ctx context.Context, sshConfig *ssh.SSHConfig, port int, command ...string) error {
	args := sshConfig.Args()
	args = append(args,
		"-p", strconv.Itoa(port),
		"127.0.0.1",
		"--",
	)
	args = append(args, command...)
	cmd := exec.CommandContext(ctx, sshConfig.Binary(), args...)
	if out, err := cmd.Output(); err != nil {
		return fmt.Errorf("failed to run %v: %q: %w", cmd.Args, string(out), err)
	}
	return nil
}

func forwardSSH(ctx context.Context, sshConfig *ssh.SSHConfig, port int, local, remote string, verb string, reverse bool) error {
	args := sshConfig.Args()
	args = append(args,
		"-T",
		"-O", verb,
	)
	if reverse {
		args = append(args,
			"-R", remote+":"+local,
		)
	} else {
		args = append(args,
			"-L", local+":"+remote,
		)
	}
	args = append(args,
		"-N",
		"-f",
		"-p", strconv.Itoa(port),
		"127.0.0.1",
		"--",
	)
	if strings.HasPrefix(local, "/") {
		switch verb {
		case verbForward:
			if reverse {
				logrus.Infof("Forwarding %q (host) to %q (guest)", local, remote)
				if err := executeSSH(ctx, sshConfig, port, "rm", "-f", remote); err != nil {
					logrus.WithError(err).Warnf("Failed to clean up %q (guest) before setting up forwarding", remote)
				}
			} else {
				logrus.Infof("Forwarding %q (guest) to %q (host)", remote, local)
				if err := os.RemoveAll(local); err != nil {
					logrus.WithError(err).Warnf("Failed to clean up %q (host) before setting up forwarding", local)
				}
			}
			if err := os.MkdirAll(filepath.Dir(local), 0o750); err != nil {
				return fmt.Errorf("can't create directory for local socket %q: %w", local, err)
			}
		case verbCancel:
			if reverse {
				logrus.Infof("Stopping forwarding %q (host) to %q (guest)", local, remote)
				if err := executeSSH(ctx, sshConfig, port, "rm", "-f", remote); err != nil {
					logrus.WithError(err).Warnf("Failed to clean up %q (guest) after stopping forwarding", remote)
				}
			} else {
				logrus.Infof("Stopping forwarding %q (guest) to %q (host)", remote, local)
				defer func() {
					if err := os.RemoveAll(local); err != nil {
						logrus.WithError(err).Warnf("Failed to clean up %q (host) after stopping forwarding", local)
					}
				}()
			}
		default:
			panic(fmt.Errorf("invalid verb %q", verb))
		}
	}
	cmd := exec.CommandContext(ctx, sshConfig.Binary(), args...)
	if out, err := cmd.Output(); err != nil {
		if verb == verbForward && strings.HasPrefix(local, "/") {
			if reverse {
				logrus.WithError(err).Warnf("Failed to set up forward from %q (host) to %q (guest)", local, remote)
				if err := executeSSH(ctx, sshConfig, port, "rm", "-f", remote); err != nil {
					logrus.WithError(err).Warnf("Failed to clean up %q (guest) after forwarding failed", remote)
				}
			} else {
				logrus.WithError(err).Warnf("Failed to set up forward from %q (guest) to %q (host)", remote, local)
				if removeErr := os.RemoveAll(local); err != nil {
					logrus.WithError(removeErr).Warnf("Failed to clean up %q (host) after forwarding failed", local)
				}
			}
		}
		return fmt.Errorf("failed to run %v: %q: %w", cmd.Args, string(out), err)
	}
	return nil
}

func copyToHost(ctx context.Context, sshConfig *ssh.SSHConfig, port int, local, remote string) error {
	args := sshConfig.Args()
	args = append(args,
		"-p", strconv.Itoa(port),
		"127.0.0.1",
		"--",
	)
	args = append(args,
		"sudo",
		"cat",
		remote,
	)
	logrus.Infof("Copying config from %s to %s", remote, local)
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return fmt.Errorf("can't create directory for local file %q: %w", local, err)
	}
	cmd := exec.CommandContext(ctx, sshConfig.Binary(), args...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to run %v: %q: %w", cmd.Args, string(out), err)
	}
	if err := os.WriteFile(local, out, 0o600); err != nil {
		return fmt.Errorf("can't write to local file %q: %w", local, err)
	}
	return nil
}
