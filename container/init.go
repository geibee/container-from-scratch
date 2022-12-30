package container

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/opencontainers/runc/libcontainer/system"
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
	// cmd := exec.Command(argv[0], argv[1:]...)
	// fmt.Printf("[child] cmd is %s\n", cmd)
	// cmd.Stdin = os.Stdin
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr

	// factory_linux.goのStartInitialization() Init()の前座
	// standard_init_linux.goのInit() こっちがメイン処理
	// Must(setupNetwork())
	// Must(setupRoute())
	/*
		prepareRootfs()の簡易的な実装
	*/
	flag := unix.MS_SLAVE | unix.MS_REC
	// Must(unix.Sethostname([]byte("container")))
	Must(unix.Mount("", "/", "", uintptr(flag), ""))
	Must(unix.Mount("rootfs", "/", "bind", unix.MS_BIND|unix.MS_REC, ""))
	Must(unix.Mount("proc", "/proc", "proc", 0, ""))

	// Must(syscall.Chroot("/"))
	Must(os.Chdir("/"))

	environ := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	// Must(cmd.Run())

	fmt.Printf("[child] cmd is %s\n", argv[0])
	fmt.Printf("[child] args is %s\n", argv[1:])
	return system.Exec(argv[0], argv[1:], environ)
}

/*
* TODO: replace pivotRoot
* this pivotRoot() is borrowed from libcontainer repository(https://github.com/opencontainers/runc/blob/main/libcontainer/rootfs_linux.go#L819-L874)
 */

func pivotRoot(rootfs string) error {

	oldroot, err := unix.Open("/", unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: "/", Err: err}
	}
	defer unix.Close(oldroot) //nolint: errcheck

	newroot, err := unix.Open(rootfs, unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: rootfs, Err: err}
	}
	defer unix.Close(newroot) //nolint: errcheck

	// Change to the new root so that the pivot_root actually acts on it.
	if err := unix.Fchdir(newroot); err != nil {
		return &os.PathError{Op: "fchdir", Path: "fd " + strconv.Itoa(newroot), Err: err}
	}

	if err := unix.PivotRoot(".", "."); err != nil {
		return &os.PathError{Op: "pivot_root", Path: ".", Err: err}
	}

	// Currently our "." is oldroot (according to the current kernel code).
	// However, purely for safety, we will fchdir(oldroot) since there isn't
	// really any guarantee from the kernel what /proc/self/cwd will be after a
	// pivot_root(2).

	if err := unix.Fchdir(oldroot); err != nil {
		return &os.PathError{Op: "fchdir", Path: "fd " + strconv.Itoa(oldroot), Err: err}
	}

	// Make oldroot rslave to make sure our unmounts don't propagate to the
	// host (and thus bork the machine). We don't use rprivate because this is
	// known to cause issues due to races where we still have a reference to a
	// mount while a process in the host namespace are trying to operate on
	// something they think has no mounts (devicemapper in particular).
	if err := mount("", ".", "", "", unix.MS_SLAVE|unix.MS_REC, ""); err != nil {
		return err
	}
	// Perform the unmount. MNT_DETACH allows us to unmount /proc/self/cwd.
	if err := unmount(".", unix.MNT_DETACH); err != nil {
		return err
	}

	// Switch back to our shiny new root.
	if err := unix.Chdir("/"); err != nil {
		return &os.PathError{Op: "chdir", Path: "/", Err: err}
	}
	return nil
}

/*
* TODO: replace below
* the code below is borrowed from libcontainer repository(https://github.com/opencontainers/runc/blob/main/libcontainer/mount_linux.go)
 */

// mountError holds an error from a failed mount or unmount operation.
type mountError struct {
	op     string
	source string
	target string
	procfd string
	flags  uintptr
	data   string
	err    error
}

// Error provides a string error representation.
func (e *mountError) Error() string {
	out := e.op + " "

	if e.source != "" {
		out += e.source + ":" + e.target
	} else {
		out += e.target
	}
	if e.procfd != "" {
		out += " (via " + e.procfd + ")"
	}

	if e.flags != uintptr(0) {
		out += ", flags: 0x" + strconv.FormatUint(uint64(e.flags), 16)
	}
	if e.data != "" {
		out += ", data: " + e.data
	}

	out += ": " + e.err.Error()
	return out
}

// Unwrap returns the underlying error.
// This is a convention used by Go 1.13+ standard library.
func (e *mountError) Unwrap() error {
	return e.err
}

func mount(source, target, procfd, fstype string, flags uintptr, data string) error {
	dst := target
	if procfd != "" {
		dst = procfd
	}
	if err := unix.Mount(source, dst, fstype, flags, data); err != nil {
		return &mountError{
			op:     "mount",
			source: source,
			target: target,
			procfd: procfd,
			flags:  flags,
			data:   data,
			err:    err,
		}
	}
	return nil
}

// unmount is a simple unix.Unmount wrapper.
func unmount(target string, flags int) error {
	err := unix.Unmount(target, flags)
	if err != nil {
		return &mountError{
			op:     "unmount",
			target: target,
			flags:  uintptr(flags),
			err:    err,
		}
	}
	return nil
}
