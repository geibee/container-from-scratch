package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opencontainers/runc/libcontainer/utils"
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
	fifo            *os.File
	m               sync.Mutex
}

type pid struct {
	Pid           int `json:"stage2_pid"`
	PidFirstChild int `json:"stage1_pid"`
}

const stdioFdCount = 3

func Run(ctx *cli.Context) error {
	command := ctx.String("command")
	argv := append([]string{"--command"}, command)

	fmt.Println("cfs run...")
	parentInitPipe, childInitPipe, err := NewSockPair("init")
	if err != nil {
		return fmt.Errorf("unable to create init pipe: %s", err)
	}

	messageSockPair := filePair{parentInitPipe, childInitPipe}

	fmt.Println("creating fifo")
	//TODO: 本当はrootオプションの直下に作成する
	fifoName := "tmp/exec.fifo"
	if _, err := os.Stat(fifoName); err == nil {
		return fmt.Errorf("exec fifo %s already exists", fifoName)
	}

	oldMask := unix.Umask(0o000)
	if err := unix.Mkfifo(fifoName, 0o622); err != nil {
		unix.Umask(oldMask)
		return err
	}
	unix.Umask(oldMask)
	// if err := os.Chown(fifoName, 1, 1); err != nil {
	// 	return fmt.Errorf("exec fifo %s chown failed", fifoName)
	// }

	// newParentProcess()の中で、fifoを開いてそれをExtraFilesに加えたcmdが作成される。
	fmt.Println("opening fifo")
	fifo, err := os.OpenFile(fifoName, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("exec fifo %s open failed", fifoName)
	}
	args := append([]string{"init"}, argv...)
	cmd := exec.Command("/proc/self/exe", args...)
	fmt.Printf("init command option is %s \n", args)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// stdin, stdout, stderr, childInitPipe, fifofdの順にfdを渡す
	// Socketpairの子供側をExtraFilesの末尾に追加し、そのfdを環境変数に渡す
	cmd.ExtraFiles = append(cmd.ExtraFiles, childInitPipe)
	cmd.Env = append(cmd.Env,
		"_LIBCONTAINER_INITPIPE="+strconv.Itoa(stdioFdCount+len(cmd.ExtraFiles)-1))
	// fifoをExtraFilesの末尾に追加し、そのfdを環境変数に渡す
	cmd.ExtraFiles = append(cmd.ExtraFiles, fifo)
	cmd.Env = append(cmd.Env,
		"_LIBCONTAINER_FIFOFD="+strconv.Itoa(stdioFdCount+len(cmd.ExtraFiles)-1))

	init := &initProcess{
		cmd:             cmd,
		messageSockPair: messageSockPair,
		fifo:            fifo,
	}

	// if err := init.start(); err != nil {
	// 	os.Remove(fifoName)
	// 	return err
	// }
	if err := Start(init); err != nil {
		return err
	}

	fifo.Close()
	blockingFifoOpenCh := awaitFifoOpen(fifoName)
	for {
		select {
		case result := <-blockingFifoOpenCh:
			return handleFifoResult(result)
		case <-time.After(time.Microsecond * 100):
			if err := handleFifoResult(fifoOpen(fifoName, false)); err != nil {
				return errors.New("container process is already dead")
			}
			return nil
		}
	}

}

type openResult struct {
	file *os.File
	err  error
}

func awaitFifoOpen(path string) <-chan openResult {
	fifoOpened := make(chan openResult)
	go func() {
		result := fifoOpen(path, true)
		fifoOpened <- result
	}()
	return fifoOpened
}

func fifoOpen(path string, block bool) openResult {
	flags := os.O_RDONLY
	if !block {
		flags |= unix.O_NONBLOCK
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return openResult{err: fmt.Errorf("exec fifo: %w", err)}
	}
	return openResult{file: f}
}

func handleFifoResult(result openResult) error {
	if result.err != nil {
		return result.err
	}
	f := result.file
	defer f.Close()
	if err := readFromExecFifo(f); err != nil {
		return err
	}
	return os.Remove(f.Name())
}

func readFromExecFifo(execFifo io.Reader) error {
	data, err := io.ReadAll(execFifo)
	if err != nil {
		return err
	}
	if len(data) <= 0 {
		return errors.New("cannot start an already running container")
	}
	return nil
}

/*
  cfs run(=runc run)から、cfs init(=runc init)を実行する
*/

func (p *initProcess) start() error {
	defer p.messageSockPair.parent.Close()
	err := p.cmd.Start()
	//Tips: 親側のプロセスではSocketpairの子側はいらないから閉じる
	_ = p.messageSockPair.child.Close()
	if err != nil {
		return fmt.Errorf("initProcess start failed: %w", err)
	}

	//Tips: 子側がSockpairに何か送ってきてるか確認し、何も送ってきてなかったらエラーにする
	waitInit := initWaiter(p.messageSockPair.parent)
	err = <-waitInit
	defer func() {
		if err != nil {
			fmt.Printf("child side sends nothing")
		}
	}()
	if err != nil {
		return err
	}

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
	return nil
}

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
func Initialization(ctx *cli.Context) error {
	command := ctx.String("command")
	fmt.Printf("Running %v \n", command)
	argv := strings.Split(command, " ")
	fmt.Printf("split argv is %s\n", argv)

	envInitPipe := os.Getenv("_LIBCONTAINER_INITPIPE")
	pipefd, err := strconv.Atoi(envInitPipe)
	if err != nil {
		return fmt.Errorf("unable to convert _LIBCONTAINER_INITPIPE: %w", err)
	}
	pipe := os.NewFile(uintptr(pipefd), "pipe")
	defer pipe.Close()

	//QUESTIONING pipe.Close()の直前に、initPipeにprocErrorを書き込む。fdは-1。なんで？
	defer func() {
		if werr := writeSyncWithFd(pipe, procError, -1); werr != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if werr := utils.WriteJSON(pipe, &initError{Message: err.Error()}); werr != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
	}()

	/* QUESTIONING なんでfdを-1にするのか
	*  initではなくsetnsの場合は、fifofdは使わない。それを明示するために？-1を入れてるのかな。
	*  どうせfifofdには値が代入されるし。
	 */
	fifofd := -1
	envFifoFd := os.Getenv("_LIBCONTAINER_FIFOFD")
	fmt.Println("fifofd setting finished")
	//TODO: add console socket
	//TODO: add logpipe
	//TODO: parse mount fd

	// os.Clearenv()

	/*==============以降、standard_init_linux.go Init()の再現 =================
	*
	 */

	// 親プロセスとつながったSocketPairの子供側に書き込む。これによって、runc runの親プロセスのソケットが先に進む
	if err := writeSyncWithFd(pipe, procReady, -1); err != nil {
		return fmt.Errorf("sync ready: %w", err)
	}
	_ = pipe.Close()
	fmt.Printf("setting /proc/self/fd/%s\n", envFifoFd)
	fifoPath := "/proc/self/fd/" + envFifoFd
	// Tips: ここで、fifoがもう一方から開かれるのを待ち受ける。
	fd, err := unix.Open(fifoPath, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		fmt.Println("open failed")
		return &os.PathError{Op: "open exec fifo", Path: fifoPath, Err: err}
	}
	fmt.Println("sending fifofd 0")
	if _, err := unix.Write(fd, []byte("0")); err != nil {
		fmt.Println("write failed")
		return &os.PathError{Op: "write exec fifo", Path: fifoPath, Err: err}
	}
	fmt.Println("/proc/self/fd closing")
	_ = unix.Close(fifofd)
	//TODO: setupnetwork
	//TODO: setuproute
	//TODO: prepareRootfs
	//TODO: createConsole
	cmd := exec.Command(argv[0], argv[1:]...)
	fmt.Printf("cmd is %s\n", cmd)
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
	return nil
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

type syncType string

const (
	procError syncType = "procError"
	procReady syncType = "procReady"
)

type syncT struct {
	Type syncType `json:"type"`
	Fd   int      `json:"fd"`
}

func writeSyncWithFd(pipe io.Writer, sync syncType, fd int) error {
	if err := utils.WriteJSON(pipe, syncT{sync, fd}); err != nil {
		return fmt.Errorf("writing syncT %q: %w", string(sync), err)
	}
	return nil
}

type initError struct {
	Message string `json:"message,omitempty"`
}

func Start(process *initProcess) error {
	process.m.Lock()
	defer process.m.Unlock()
	if err := process.start(); err != nil {
		os.Remove("tmp/exec.fifo")
		return err
	}
	return nil
}
