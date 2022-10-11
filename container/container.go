package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
)

type filePair struct {
	parent *os.File
	child  *os.File
}

type initProcess struct {
	cmd             *exec.Cmd
	messageSockPair filePair
	fds             []string
}

type pid struct {
	Pid           int `json:"stage2_pid"`
	PidFirstChild int `json:"stage1_pid"`
}

const stdioFdCount = 3

func Run(ctx *cli.Context) {
	command := ctx.String("command")
	argv := append([]string{"--command"}, command)

	fmt.Printf("cfs run... \n")
	parentInitPipe, childInitPipe, err := NewSockPair("init")
	if err != nil {
		fmt.Printf("unable to create init pipe")
		panic(err)
	}

	messageSockPair := filePair{parentInitPipe, childInitPipe}
	args := append([]string{"init"}, argv...)
	cmd := exec.Command("/proc/self/exe", args...)
	fmt.Printf("init command option is %s \n", args)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = append(cmd.ExtraFiles, childInitPipe)
	cmd.Env = append(cmd.Env,
		"_LIBCONTAINER_INITPIPE="+strconv.Itoa(stdioFdCount+len(cmd.ExtraFiles)-1))

	init := &initProcess{
		cmd:             cmd,
		messageSockPair: messageSockPair,
	}
	init.start()
}

/*
  cfs run(=runc run)から、cfs init(=runc init)を実行する
*/
func (p *initProcess) start() {
	Must(p.cmd.Run())
}

/*
func (p *initProcess) start() {
	defer p.messageSockPair.parent.Close()
	err := p.cmd.Start()
	_ = p.messageSockPair.child.Close()
	if err != nil {
		fmt.Printf("initProcess start failed: %s", err)
		panic(err)
	}
	// waitInit := initWaiter(p.messageSockPair.parent)
	// err = <-waitInit
	// if err != nil {
	// 	panic(err)
	// }

	childPid, err := p.getChildPid()
	if err != nil {
		fmt.Printf("childPid: %s\n", err)
		panic(err)
	}
	fds, err := getPipeFds(childPid)
	if err != nil {
		fmt.Printf("error getting pipe fds for pid: %d", childPid)
		panic(err)
	}
	p.fds = fds
}
*/

func initWaiter(r io.Reader) chan error {
	ch := make(chan error, 1)
	go func() {
		defer close(ch)
		inited := make([]byte, 1)
		n, err := r.Read(inited)
		if err == nil {
			if n < 1 {
				err = errors.New("short read")
			} else if inited[0] != 0 {
				err = fmt.Errorf("unexpected %d != 0", inited[0])
			} else {
				ch <- nil
				return
			}
		}
		ch <- fmt.Errorf("waiting for init preliminary setup: %w", err)
	}()
	return ch
}

func (p *initProcess) getChildPid() (int, error) {
	var pid pid
	if err := json.NewDecoder(p.messageSockPair.parent).Decode(&pid); err != nil {
		_ = p.cmd.Wait()
		return -1, err
	}

	firstChildProcess, _ := os.FindProcess(pid.PidFirstChild)
	_, _ = firstChildProcess.Wait()
	return pid.Pid, nil
}

func getPipeFds(pid int) ([]string, error) {
	fds := make([]string, 3)

	dirPath := filepath.Join("/proc", strconv.Itoa(pid), "/fd")
	for i := 0; i < 3; i++ {
		f := filepath.Join(dirPath, strconv.Itoa(i))
		target, err := os.Readlink(f)
		if err != nil {
			if os.IsPermission(err) {
				continue
			}
			return fds, err
		}
		fds[i] = target
	}
	return fds, nil
}

func setupNetwork() {

}

// cfs init(=runc init)の実体
func Initialization(ctx *cli.Context) {
	command := ctx.String("command")
	fmt.Printf("Running %v \n", command)
	argv := strings.Split(command, " ")
	fmt.Printf("split argv is %s\n", argv)

	envInitPipe := os.Getenv("_LIBCONTAINER_INITPIPE")
	pipefd, err := strconv.Atoi(envInitPipe)
	if err != nil {
		err = fmt.Errorf("unable to convert _LIBCONTAINER_INITPIPE: %w", err)
		fmt.Printf("convert %s", err)
		panic(err)
	}
	pipe := os.NewFile(uintptr(pipefd), "pipe")
	defer pipe.Close()
	cg()

	// var consoleSocket *os.File
	// if envConsole := os.Getenv("_LIBCONTAINER_CONSOLE"); envConsole != "" {
	// 	console, err := strconv.Atoi(envConsole)
	// 	if err != nil {
	// 		return fmt.Errorf("unable to convert _LIBCONTAINER_CONSOLE: %w", err)
	// 	}
	// 	consoleSocket = os.NewFile(uintptr(console), "console-socket")
	// 	defer consoleSocket.Close()
	// }

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// factory_linux.goのStartInitialization() Init()の前座
	// standard_init_linux.goのInit() こっちがメイン処理
	// Must(setupNetwork())
	// Must(setupRoute())
	/*
		prepareRootfs()の簡易的な実装
	*/
	flag := unix.MS_SLAVE | unix.MS_REC
	Must(unix.Sethostname([]byte("container")))
	Must(unix.Mount("", "/", "", uintptr(flag), ""))
	Must(unix.Mount("rootfs", "/", "bind", unix.MS_BIND|unix.MS_REC, ""))
	Must(unix.Mount("proc", "/proc", "proc", 0, ""))
	Must(syscall.Chroot("/"))
	Must(os.Chdir("/"))
	Must(cmd.Run())
	/*
		簡単な方の実装
		// Must(syscall.Sethostname([]byte("container")))
		// Must(syscall.Chroot("/"))
		// Must(os.Chdir("/"))
		// Must(syscall.Mount("proc", "proc", "proc", 0, ""))
		// Must(cmd.Run())
		// Must(syscall.Unmount("proc", 0))
	*/

	/*
		Init()の中の最後の処理
		fifoPath := "/proc/self/fd/" + strconv.Itoa(l.fifoFd)
		fd, err := unix.Open(fifoPath, unix.O_WRONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return &os.PathError{Op: "open exec fifo", Path: fifoPath, Err: err}
		}
		if _, err := unix.Write(fd, []byte("0")); err != nil {
			return &os.PathError{Op: "write exec fifo", Path: fifoPath, Err: err}
		}

		_ = unix.Close(l.fifoFd)

		s := l.config.SpecState
		s.Pid = unix.Getpid()
		s.Status = specs.StateCreated
		if err := l.config.Config.Hooks[configs.StartContainer].RunHooks(s); err != nil {
			return err
		}

		return system.Exec(name, l.config.Args[0:], os.Environ())
	*/
}

func cg() {
	cgroups := "/sys/fs/cgroup/"
	pids := filepath.Join(cgroups, "pids")
	cgPath := filepath.Join(pids, "cfs")
	if !Exists(cgPath) {
		Must(os.Mkdir(filepath.Join(pids, "cfs"), 0755))
	}
	Must(ioutil.WriteFile(filepath.Join(pids, "cfs/pids.max"), []byte("20"), 0700))
	Must(ioutil.WriteFile(filepath.Join(pids, "cfs/notify_on_release"), []byte("1"), 0700))
	Must(ioutil.WriteFile(filepath.Join(pids, "cfs/cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0700))
}

func Must(err error) {
	if err != nil {
		panic(err)
	}
}

func Exists(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

/* from "github.com/opencontainers/runc/libcontainer/utils"
   AF_LOCAL: communication between the same machine
   SOCK_STREAM: stream socket. that means the socketpair deals with TCP
   SOCK_CLOEXEC: close fd after execute execve()
*/
func NewSockPair(name string) (parent *os.File, child *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_LOCAL, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[1]), name+"-p"), os.NewFile(uintptr(fds[0]), name+"-c"), nil
}
