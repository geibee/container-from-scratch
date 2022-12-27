package container

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
)

// cfs init(=runc init)の実体
func Initialization(ctx *cli.Context) error {
	command := ctx.String("command")
	fmt.Printf("[child] Running %v \n", command)
	argv := strings.Split(command, " ")
	fmt.Printf("[child] split argv is %s\n", argv)

	envInitPipe := os.Getenv("_LIBCONTAINER_INITPIPE")
	pipefd, err := strconv.Atoi(envInitPipe)
	if err != nil {
		return fmt.Errorf("[child] unable to convert _LIBCONTAINER_INITPIPE: %w", err)
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
	fmt.Println("[child] fifofd setting finished")
	//TODO: add console socket
	//TODO: add logpipe
	//TODO: parse mount fd

	// os.Clearenv()

	/*==============以降、standard_init_linux.go Init()の再現 =================
	*
	 */

	//TODO: setupnetwork
	//TODO: setuproute
	// prepareRootfs: これをやらなければ、procHooksがchildから送られない。

	//TODO: createConsole

	// 親プロセスとつながったSocketPairの子供側に書き込む。これによって、runc runの親プロセスのソケットが先に進む
	if err := writeSyncWithFd(pipe, procReady, -1); err != nil {
		fmt.Printf("[child] error is %s\n", err)
		return fmt.Errorf("[child] sync ready: %w", err)
	}
	fmt.Println("[child] wrote to child side of sockpair")
	// _ = pipe.Close()
	fifoPath := "/proc/self/fd/" + envFifoFd
	// Tips: ここで、fifoがもう一方から開かれるのを待ち受ける。
	fmt.Println("[child] opening fifo")
	fd, err := unix.Open(fifoPath, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		fmt.Println("[child] open failed")
		return &os.PathError{Op: "[child] open exec fifo", Path: fifoPath, Err: err}
	}
	fmt.Println("[child] sending fifofd 0")
	if _, err := unix.Write(fd, []byte("0")); err != nil {
		fmt.Println("[child] write failed")
		return &os.PathError{Op: "[child] write exec fifo", Path: fifoPath, Err: err}
	}
	fmt.Println("[child] /proc/self/fd closing")
	_ = unix.Close(fifofd)
	cmd := exec.Command(argv[0], argv[1:]...)
	fmt.Printf("[child] cmd is %s\n", cmd)
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
