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
	"sync"
	"time"

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

	fmt.Println("[parent] cfs run...")
	parentInitPipe, childInitPipe, err := NewSockPair("init")
	if err != nil {
		return fmt.Errorf("[parent] unable to create init pipe: %s", err)
	}

	messageSockPair := filePair{parentInitPipe, childInitPipe}

	fmt.Println("[parent] creating fifo")
	//TODO: 本当はrootオプションの直下に作成する
	fifoName := "tmp/exec.fifo"
	if _, err := os.Stat(fifoName); err == nil {
		return fmt.Errorf("[parent] exec fifo %s already exists", fifoName)
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
	fmt.Println("[parent] opening fifo")
	fifo, err := os.OpenFile(fifoName, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("[parent] exec fifo %s open failed", fifoName)
	}
	args := append([]string{"init"}, argv...)
	cmd := exec.Command("/proc/self/exe", args...)
	fmt.Printf("[parent] init command option is %s \n", args)
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
				return errors.New("[parent] container process is already dead")
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
		return openResult{err: fmt.Errorf("[parent] exec fifo: %w", err)}
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
		return errors.New("[parent] cannot start an already running container")
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
		return fmt.Errorf("[parent] initProcess start failed: %w", err)
	}

	//Tips: 子側がSockpairに何か送ってきてるか確認し、何も送ってきてなかったらエラーにする
	waitInit := initWaiter(p.messageSockPair.parent)

	// bootstrapdata stub
	// testrd := os.NewFile(uintptr(5), "test-reader")
	// testrd.Write([]byte("0"))
	// if _, err := io.Copy(p.messageSockPair.parent, testrd); err != nil {
	// 	fmt.Println("io.Copy")
	// 	return fmt.Errorf("can't copy bootstrap data to pipe: %w", err)
	// }
	err = <-waitInit
	defer func() {
		if err != nil {
			fmt.Printf("[parent] child side sends nothing")
		}
	}()
	if err != nil {
		fmt.Println("[parent] initwaiter fails")
		return err
	}
	fmt.Println("[parent] initwaiter() ok!")

	childPid, err := p.getChildPid()
	if err != nil {
		fmt.Printf("[parent] childPid: %s\n", err)
		panic(err)
	}
	fmt.Println("[parent] getChildPid() ok!")
	fds, err := getPipeFds(childPid)
	if err != nil {
		fmt.Printf("[parent] error getting pipe fds for pid: %d\n", childPid)
		panic(err)
	}
	fmt.Println("[parent] getPipeFds() ok!")
	p.fds = fds

	err = p.cmd.Wait()
	if err != nil {
		fmt.Println("[parent] p.cmd.Wait() failed.")
		return err
	}

	if err := unix.Shutdown(int(p.messageSockPair.parent.Fd()), unix.SHUT_WR); err != nil {
		return &os.PathError{Op: "shutdown", Path: "(init pipe)", Err: err}
	}

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
				fmt.Println("[parent] short read")
				err = errors.New("[parent] short read")
			} else if inited[0] != 0 {
				fmt.Printf("[parent] init[0] is %s\n", inited[0])
				err = fmt.Errorf("[parent] unexpected %d != 0", inited[0])
			} else {
				fmt.Println("[parent] ok")
				ch <- nil
				return
			}
		}
		ch <- fmt.Errorf("[parent] waiting for init preliminary setup: %w", err)
	}()
	return ch
}

func (p *initProcess) getChildPid() (int, error) {
	var pid pid
	if err := json.NewDecoder(p.messageSockPair.parent).Decode(&pid); err != nil {
		fmt.Printf("[parent] getChildPid()'s error: %v\n", err)
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
	procHooks syncType = "procHooks"
)

type syncT struct {
	Type syncType `json:"type"`
	Fd   int      `json:"fd"`
}

func writeSyncWithFd(pipe io.Writer, sync syncType, fd int) error {
	inited := make([]byte, 2)
	inited[0] = 0
	inited[1] = 1
	if _, err := pipe.Write(inited); err != nil {
		return fmt.Errorf("[parent] writing syncT %q: %w", string(sync), err)
	}
	// if err := utils.WriteJSON(pipe, syncT{sync, fd}); err != nil {
	// return fmt.Errorf("[parent] writing syncT %q: %w", string(sync), err)
	// }
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
