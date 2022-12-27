package container

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink/nl"
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
	bootstrapData   io.Reader
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

	var (
		namespaces       configs.Namespaces
		namespaceMapping map[string]configs.NamespaceType
	)

	// runcではLinuxNamespaceTypeをNamespaceTypeに変換するために存在する。
	// linux以外で使う想定がないので簡略化
	namespaceMapping = map[string]configs.NamespaceType{
		"PID":     configs.NEWPID,
		"Network": configs.NEWNET,
		"Mount":   configs.NEWNS,
		"User":    configs.NEWUSER,
		"IPC":     configs.NEWIPC,
		"UTS":     configs.NEWUTS,
		"Cgroup":  configs.NEWCGROUP,
	}

	for _, nsType := range []string{"PID", "Network", "IPC", "UTS", "MOUNT"} {
		// Goでは、Mapのキーを参照すると値とキーの存在有無のboolが返却される
		t, _ := namespaceMapping[nsType]
		// 本当はLinuxのNamespaceのPathはnetだったりmntだったりする
		// LinuxNamespace型を作るのが面倒なので、mountとnetworkディレクトリでよしとする
		// 20221223追記: ns/networkはnsexec()に怒られるので、netにする
		if nsType == "Network" {
			namespaces.Add(t, "/proc/self/ns/net")
		} else {
			namespaces.Add(t, "/proc/self/ns/"+strings.ToLower(nsType))
		}
	}

	// runcでデフォルトで設定されるnamespaceをmapにする
	nsMaps := make(map[configs.NamespaceType]string)
	for _, ns := range namespaces {
		if ns.Path != "" {
			nsMaps[ns.Type] = ns.Path
		}
	}

	// TODO: CloneFlagsが無効な値になっている可能性が高い
	data, err := bootstrapData(namespaces.CloneFlags(), nsMaps, initStandard)

	init := &initProcess{
		cmd:             cmd,
		messageSockPair: messageSockPair,
		fifo:            fifo,
		bootstrapData:   data,
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
	if _, err := io.Copy(p.messageSockPair.parent, p.bootstrapData); err != nil {
		return fmt.Errorf("[parent] can't copy bootstrap data to pipe: %w", err)
	}
	fmt.Printf("[parent] copy from bootstrapData to parent sockpair ok!")

	err = <-waitInit
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

	err = p.waitForChildExit(childPid)
	if err != nil {
		return fmt.Errorf("[parent] error waiting for our first child to exit: %w", err)
	}

	ierr := parseSync(p.messageSockPair.parent, func(sync *syncT) error {
		switch sync.Type {
		case procHooks:
			fmt.Println("[parent] case procHooks")
			return nil
		case procReady:
			fmt.Println("[parent] case procReady")
			return nil
		default:
			fmt.Println("[parent] case default")
			return errors.New("invalid JSON payload from child")
		}
	})

	if ierr != nil {
		fmt.Println("[parent] parseSync failed: ierr isn't nil")
		_ = p.cmd.Wait()
		return ierr
	}

	fmt.Println("[parent] unix Shutdown")
	if err := unix.Shutdown(int(p.messageSockPair.parent.Fd()), unix.SHUT_WR); err != nil {
		return &os.PathError{Op: "shutdown", Path: "(init pipe)", Err: err}
	}

	return nil
}

func parseSync(pipe io.Reader, fn func(*syncT) error) error {
	fmt.Println("[parent] running parseSync - reading from pipe")
	dec := json.NewDecoder(pipe)
	for {
		var sync syncT
		if err := dec.Decode(&sync); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			fmt.Printf("[parent] running parseSync - error is %s\n", err)
			return err
		}

		if err := fn(&sync); err != nil {
			return err
		}
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

func (p *initProcess) waitForChildExit(childPid int) error {
	status, err := p.cmd.Process.Wait()
	if err != nil {
		_ = p.cmd.Wait()
		return err
	}
	if !status.Success() {
		_ = p.cmd.Wait()
		return &exec.ExitError{ProcessState: status}
	}

	process, err := os.FindProcess(childPid)
	if err != nil {
		return err
	}
	p.cmd.Process = process
	// p.process.ops = p
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
	procHooks syncType = "procHooks"
)

type syncT struct {
	Type syncType `json:"type"`
	Fd   int      `json:"fd"`
}

func writeSyncWithFd(pipe io.Writer, sync syncType, fd int) error {
	inited := make([]byte, 1)
	inited[0] = 0
	if err := utils.WriteJSON(pipe, syncT{sync, fd}); err != nil {
		return fmt.Errorf("[parent] writing syncT %q: %w", string(sync), err)
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

type initType string

const (
	initSetns    initType = "setns"
	initStandard initType = "standard"
)

const (
	InitMsg          uint16 = 62000
	CloneFlagsAttr   uint16 = 27281
	NsPathsAttr      uint16 = 27282
	UidmapAttr       uint16 = 27283
	GidmapAttr       uint16 = 27284
	SetgroupAttr     uint16 = 27285
	OomScoreAdjAttr  uint16 = 27286
	RootlessEUIDAttr uint16 = 27287
	UidmapPathAttr   uint16 = 27288
	GidmapPathAttr   uint16 = 27289
	MountSourcesAttr uint16 = 27290
)

type Int32msg struct {
	Type  uint16
	Value uint32
}

// netlinkErrorでerrorをラップすることで、recover()がpanicを制御できるようにする
type netlinkError struct{ error }

// AddData()の引数のNetlinkRequestDataインターフェースを満たすためには、Serialize()とLen()を実装する必要がある
func (msg *Int32msg) Serialize() []byte {
	buf := make([]byte, msg.Len())
	native := nl.NativeEndian()
	native.PutUint16(buf[0:2], uint16(msg.Len()))
	native.PutUint16(buf[2:4], msg.Type)
	native.PutUint32(buf[4:8], msg.Value)
	return buf
}

func (msg *Int32msg) Len() int {
	return unix.NLA_HDRLEN + 4
}

type Bytemsg struct {
	Type  uint16
	Value []byte
}

// AddData()の引数のNetlinkRequestDataインターフェースを満たすためには、Serialize()とLen()を実装する必要がある
func (msg *Bytemsg) Serialize() []byte {
	l := msg.Len()
	if l > math.MaxUint16 {
		// We cannot return nil nor an error here, so we panic with
		// a specific type instead, which is handled via recover in
		// bootstrapData.
		panic(netlinkError{fmt.Errorf("netlink: cannot serialize bytemsg of length %d (larger than UINT16_MAX)", l)})
	}
	buf := make([]byte, (l+unix.NLA_ALIGNTO-1) & ^(unix.NLA_ALIGNTO-1))
	native := nl.NativeEndian()
	native.PutUint16(buf[0:2], uint16(l))
	native.PutUint16(buf[2:4], msg.Type)
	copy(buf[4:], msg.Value)
	return buf
}

func (msg *Bytemsg) Len() int {
	return unix.NLA_HDRLEN + len(msg.Value) + 1 // null-terminated
}

func bootstrapData(cloneFlags uintptr, nsMaps map[configs.NamespaceType]string, it initType) (_ io.Reader, Err error) {
	// TODO: これが何をしているのかをちゃんと理解する
	r := nl.NewNetlinkRequest(int(InitMsg), 0)

	// netlinkErrorが発生したときにpanicさせず、errとして返却させる
	defer func() {
		// recover()はpanicしたgoroutineのふるまいを管理できるようにする。
		// ここでは、netlinkErrorだったらErrとして返却する
		if r := recover(); r != nil {
			fmt.Println("[parent] recover()")
			if e, ok := r.(netlinkError); ok {
				fmt.Printf("[parent] netlinkError %s\n", e)
				Err = e.error
			} else {
				panic(r)
			}
		}
	}()

	fmt.Printf("[parent] cloneFlags in bootstrapData(): %d\n", uint32(cloneFlags))
	// CloneFlagをreaderに送る
	r.AddData(&Int32msg{
		Type:  CloneFlagsAttr,
		Value: uint32(cloneFlags),
	})

	// namespaceのPathをコンマ区切りで連結する
	paths := []string{}
	for _, ns := range configs.NamespaceTypes() {
		if p, ok := nsMaps[ns]; ok && p != "" {
			paths = append(paths, fmt.Sprintf("%s:%s", configs.NsName(ns), nsMaps[ns]))
		}
	}

	// コンマ区切りで連結したnamespaceのPathをreaderに送る
	r.AddData(&Bytemsg{
		Type:  NsPathsAttr,
		Value: []byte(strings.Join(paths, ",")),
	})

	// TODO: Bind Mountの設定
	fmt.Printf("[parent] r.Seliarize() in bootstrapData() is %s\n", string(r.Serialize()))
	return bytes.NewReader(r.Serialize()), nil
}
