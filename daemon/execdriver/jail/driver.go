package jail

import (
	"fmt"
	"io/ioutil"
	//"log"
	"os"
	"os/exec"
	//"path"
	//"runtime"
	"syscall"

	"github.com/docker/docker/daemon/execdriver"

	"github.com/Sirupsen/logrus"

	"github.com/docker/docker/pkg/term"
	"github.com/kr/pty"
	"io"

	"errors"
	"strings"

	"bytes"
	"strconv"
)

const DriverName = "jail"
const Version = "0.1"

func init() {
	//
	// TODO: autoload linux and linux64 kernel modules
	//
	// execdriver.RegisterInitFunc(DriverName, func(args *execdriver.InitArgs) error {
	// 	runtime.LockOSThread()

	// 	path, err := exec.LookPath(args.Args[0])
	// 	if err != nil {
	// 		log.Printf("Unable to locate %v", args.Args[0])
	// 		os.Exit(127)
	// 	}
	// 	if err := syscall.Exec(path, args.Args, os.Environ()); err != nil {
	// 		return fmt.Errorf("dockerinit unable to execute %s - %s", path, err)
	// 	}
	// 	panic("Unreachable")
	// })
}

type driver struct {
	root     string
	initPath string
}

func NewDriver(root, initPath string) (*driver, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}

	return &driver{
		root:     root,
		initPath: initPath,
	}, nil
}

func (d *driver) Name() string {
	return DriverName
}

func copyFile(src string, dest string) error {
	content, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(dest, content, 0755)
	if err != nil {
		return err
	}

	return nil
}

func (d *driver) Run(c *execdriver.Command, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (execdriver.ExitStatus, error) {
	var (
		term execdriver.Terminal
		err  error
	)

	// setting terminal parameters
	if c.ProcessConfig.Tty {
		term, err = NewTtyConsole(&c.ProcessConfig, pipes)
	} else {
		term, err = execdriver.NewStdConsole(&c.ProcessConfig, pipes)
	}
	if err != nil {
		return execdriver.ExitStatus{ExitCode: -1}, err
	}
	c.ProcessConfig.Terminal = term

	logrus.Info("[jail] running jail")

	root := c.Rootfs

	// build params for the jail
	params := []string{
		"/usr/sbin/jail",
		"-c",
		"name=" + c.ID,
		"path=" + root,
		"mount.devfs=1",
		"allow.raw_sockets=1", // TODO: this must be put in an option
	}

	// TODO: there must be a better way to detect linux
	if out, err := exec.Command(root, "/bin/sh -c 'uname -s'").Output(); string(out) == "Linux" {
		if err != nil {
			logrus.Warnf("Probing binary type for %s failed", c.ProcessConfig.Entrypoint)
		} else {
			params = append(params,
				"mount='linprocfs "+root+"/proc linprocfs rw 0 0'",
				"mount='linsysfs "+root+"/sys linsysfs rw 0 0'",
			)
		}
	}

	if c.Network.Interface != nil {
		// for some reason if HostNetworking is enabled, c.Network doesnt contain interface name and ip
		if !c.Network.HostNetworking {
			params = append(params,
				"interface="+c.Network.Interface.Bridge,
				"ip4.addr="+fmt.Sprintf("%s/%d", c.Network.Interface.IPAddress, c.Network.Interface.IPPrefixLen),
			)
		}
	} else {
		logrus.Debug("[jail] networking is disabled")
	}

	params = append(params, "command="+c.ProcessConfig.Entrypoint)
	params = append(params, c.ProcessConfig.Arguments...)

	c.ProcessConfig.Path = "/usr/sbin/jail"
	c.ProcessConfig.Args = params

	logrus.Debugf("[jail] jail params %s", params)

	if err := c.ProcessConfig.Start(); err != nil {
		logrus.Infof("jail failed %s", err)
		return execdriver.ExitStatus{ExitCode: -1}, err
	}

	logrus.Debug("[jail] jail started")

	var (
		waitErr  error
		waitLock = make(chan struct{})
	)

	go func() {
		if err := c.ProcessConfig.Wait(); err != nil {
			if _, ok := err.(*exec.ExitError); !ok { // Do not propagate the error if it's simply a status code != 0
				waitErr = err
			}
		}
		close(waitLock)
	}()

	var pid int

	c.ContainerPid = pid

	if startCallback != nil {
		startCallback(&c.ProcessConfig, pid)
	}

	<-waitLock
	exitCode := getExitCode(c)

	if err := exec.Command("umount", root+"/dev").Run(); err != nil {
		logrus.Debugf("umount %s failed: %s", c.ID, err)
	}
	exec.Command("umount", root+"/proc").Run()
	exec.Command("umount", root+"/sys").Run()

	return execdriver.ExitStatus{ExitCode: exitCode, OOMKilled: false}, waitErr
}

func (d *driver) Exec(c *execdriver.Command, processConfig *execdriver.ProcessConfig, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (int, error) {
	var (
		term execdriver.Terminal
		err  error
	)

	// setting terminal parameters
	if processConfig.Tty {
		term, err = NewTtyConsole(processConfig, pipes)
	} else {
		term, err = execdriver.NewStdConsole(processConfig, pipes)
	}
	if err != nil {
		return -1, err
	}
	processConfig.Terminal = term

	logrus.Info("[jail] running jexec")

	// build params for the jail
	params := []string{
		"/usr/sbin/jexec",
		c.ID,
		processConfig.Entrypoint,
	}

	params = append(params, processConfig.Arguments...)

	processConfig.Path = "/usr/sbin/jexec"
	processConfig.Args = params

	logrus.Debugf("[jail] jexec params %s", params)

	if err := processConfig.Start(); err != nil {
		logrus.Infof("jexec failed %s", err)
		return -1, err
	}

	logrus.Debug("jexec started")

	var (
		waitErr  error
		waitLock = make(chan struct{})
	)

	go func() {
		if err := processConfig.Wait(); err != nil {
			if _, ok := err.(*exec.ExitError); !ok { // Do not propagate the error if it's simply a status code != 0
				waitErr = err
			}
		}
		close(waitLock)
	}()

	var pid int

	c.ContainerPid = pid

	if startCallback != nil {
		startCallback(processConfig, pid)
	}

	<-waitLock
	exitCode := getExitCode(c)

	return exitCode, waitErr
}

func getExitCode(c *execdriver.Command) int {
	if c.ProcessConfig.ProcessState == nil {
		return -1
	}
	return c.ProcessConfig.ProcessState.Sys().(syscall.WaitStatus).ExitStatus()
}

func (d *driver) Kill(c *execdriver.Command, sig int) error {
	// FIXME: should be this replaced with killall\pkill?
	// NOTE: a bug is possible if using kill - jail can exist without any processes so it will be always running
	logrus.Debugf("[jail] kill %d %s", sig, c.ID)

	if err := exec.Command("jail", "-r", c.ID).Run(); err != nil {
		return err
	}

	return nil
}

func (d *driver) Pause(c *execdriver.Command) error {
	return errors.New("pause is not supported for jail execdriver")
}

func (d *driver) Unpause(c *execdriver.Command) error {
	return errors.New("pause is not supported for jail execdriver")
}

func (d *driver) Terminate(c *execdriver.Command) error {
	if err := exec.Command("jail", "-r", c.ID).Run(); err != nil {
		return err
	}
	return nil
}

func (d *driver) GetPidsForContainer(id string) ([]int, error) {

	cmd := exec.Command("ps", "-opid", "-xaJ", id)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}

	pids := make([]int, 0)
	for {
		line, err := out.ReadString('\n')
		if err != nil {
			break
		}

		tokens := strings.Split(line, "\n")
		pid, err := strconv.Atoi(tokens[0])
		if pid == 0 {
			continue
		}

		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}

	return pids, nil
}

func (d *driver) Clean(id string) error {
	logrus.Debugf("jail clean %s", id)
	return nil
}

func (d *driver) Stats(id string) (*execdriver.ResourceStats, error) {
	logrus.Debugf("jail stats %s", id)
	return nil, nil
}

type info struct {
	ID     string
	driver *driver
}

func (d *driver) Info(id string) execdriver.Info {
	return &info{ID: id, driver: d}
}

func (info *info) IsRunning() bool {
	if err := exec.Command("jls", "-j", info.ID).Run(); err != nil {
		return true
	}

	return false
}

// ===

type TtyConsole struct {
	MasterPty *os.File
	SlavePty  *os.File
}

func NewTtyConsole(processConfig *execdriver.ProcessConfig, pipes *execdriver.Pipes) (*TtyConsole, error) {
	// lxc is special in that we cannot create the master outside of the container without
	// opening the slave because we have nothing to provide to the cmd.  We have to open both then do
	// the crazy setup on command right now instead of passing the console path to lxc and telling it
	// to open up that console.  we save a couple of openfiles in the native driver because we can do
	// this.
	ptyMaster, ptySlave, err := pty.Open()
	if err != nil {
		return nil, err
	}

	tty := &TtyConsole{
		MasterPty: ptyMaster,
		SlavePty:  ptySlave,
	}

	if err := tty.AttachPipes(&processConfig.Cmd, pipes); err != nil {
		tty.Close()
		return nil, err
	}

	processConfig.Console = tty.SlavePty.Name()

	return tty, nil
}

func (t *TtyConsole) Master() *os.File {
	return t.MasterPty
}

func (t *TtyConsole) Resize(h, w int) error {
	return term.SetWinsize(t.MasterPty.Fd(), &term.Winsize{Height: uint16(h), Width: uint16(w)})
}

func (t *TtyConsole) AttachPipes(command *exec.Cmd, pipes *execdriver.Pipes) error {
	command.Stdout = t.SlavePty
	command.Stderr = t.SlavePty

	go func() {
		if wb, ok := pipes.Stdout.(interface {
			CloseWriters() error
		}); ok {
			defer wb.CloseWriters()
		}

		io.Copy(pipes.Stdout, t.MasterPty)
	}()

	if pipes.Stdin != nil {
		command.Stdin = t.SlavePty
		command.SysProcAttr.Setctty = true

		go func() {
			io.Copy(t.MasterPty, pipes.Stdin)

			pipes.Stdin.Close()
		}()
	}
	return nil
}

func (t *TtyConsole) Close() error {
	t.SlavePty.Close()
	return t.MasterPty.Close()
}
