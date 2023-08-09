/*
MIT License

Copyright (c) 2023 [Trzsz](https://github.com/trzsz)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package trzsz

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
)

var onExitFuncs []func()

var timeNowFunc = time.Now

var linuxRuntime bool = (runtime.GOOS == "linux")
var macosRuntime bool = (runtime.GOOS == "darwin")
var windowsRuntime bool = (runtime.GOOS == "windows")
var windowsEnvironment bool = (runtime.GOOS == "windows")

func isRunningOnLinux() bool {
	return linuxRuntime
}

func isRunningOnMacOS() bool {
	return macosRuntime
}

// isRunningOnWindows returns whether the runtime platform is Windows.
func isRunningOnWindows() bool {
	return windowsRuntime
}

// isWindowsEnvironment returns false if trzsz is not affected by Windows.
func isWindowsEnvironment() bool {
	return windowsEnvironment
}

// SetAffectedByWindows set whether trzsz is affected by Windows.
func SetAffectedByWindows(affected bool) {
	windowsEnvironment = affected
}

type progressCallback interface {
	onNum(num int64)
	onName(name string)
	onSize(size int64)
	onStep(step int64)
	onDone()
	setPreSize(size int64)
	setPause(pausing bool)
}

type bufferSize struct {
	Size int64
}

type compressType int

const (
	kCompressAuto = 0
	kCompressYes  = 1
	kCompressNo   = 2
)

type baseArgs struct {
	Quiet     bool         `arg:"-q" help:"quiet (hide progress bar)"`
	Overwrite bool         `arg:"-y" help:"yes, overwrite existing file(s)"`
	Binary    bool         `arg:"-b" help:"binary transfer mode, faster for binary files"`
	Escape    bool         `arg:"-e" help:"escape all known control characters"`
	Directory bool         `arg:"-d" help:"transfer directories and files"`
	Recursive bool         `arg:"-r" help:"transfer directories and files, same as -d"`
	Bufsize   bufferSize   `arg:"-B" placeholder:"N" default:"10M" help:"max buffer chunk size (1K<=N<=1G). (default: 10M)"`
	Timeout   int          `arg:"-t" placeholder:"N" default:"20" help:"timeout ( N seconds ) for each buffer chunk.\nN <= 0 means never timeout. (default: 20)"`
	Compress  compressType `arg:"-c" placeholder:"yes/no/auto" default:"auto" help:"compress type (default: auto)"`
}

// 正则表达式:小写模式, 从头部匹配 多个数字, 从尾部匹配0或1个单位
var sizeRegexp = regexp.MustCompile(`(?i)^(\d+)(b|k|m|g|kb|mb|gb)?$`)

// 从 text 解析 buffsize
func (b *bufferSize) UnmarshalText(buf []byte) error {
	str := string(buf)
	match := sizeRegexp.FindStringSubmatch(str)
	if len(match) < 2 {
		return fmt.Errorf("invalid size %s", str)
	}
	sizeValue, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid size %s", str)
	}
	if len(match) > 2 {
		unitSuffix := strings.ToLower(match[2])
		if len(unitSuffix) == 0 || unitSuffix == "b" {
			// sizeValue *= 1
		} else if unitSuffix == "k" || unitSuffix == "kb" {
			sizeValue *= 1024
		} else if unitSuffix == "m" || unitSuffix == "mb" {
			sizeValue *= 1024 * 1024
		} else if unitSuffix == "g" || unitSuffix == "gb" {
			sizeValue *= 1024 * 1024 * 1024
		} else {
			return fmt.Errorf("invalid size %s", str)
		}
	}
	if sizeValue < 1024 {
		return fmt.Errorf("less than 1K")
	}
	if sizeValue > 1024*1024*1024 {
		return fmt.Errorf("greater than 1G")
	}
	b.Size = sizeValue
	return nil
}

// 从 text 解析 压缩类型
func (c *compressType) UnmarshalText(buf []byte) error {
	str := strings.ToLower(strings.TrimSpace(string(buf)))
	switch str {
	case "auto":
		*c = kCompressAuto
		return nil
	case "yes":
		*c = kCompressYes
		return nil
	case "no":
		*c = kCompressNo
		return nil
	default:
		return fmt.Errorf("invalid compress type %s", str)
	}
}

// 从 json 解析压缩类型
func (c *compressType) UnmarshalJSON(data []byte) error {
	var compress int
	if err := json.Unmarshal(data, &compress); err != nil {
		return err
	}
	*c = compressType(compress)
	return nil
}

// 压缩并加密字节序列
func encodeBytes(buf []byte) string {
	b := bytes.NewBuffer(make([]byte, 0, len(buf)+0x10))
	z := zlib.NewWriter(b)       // 利用 b 为底层创建压缩对象 z
	_ = writeAll(z, []byte(buf)) // 把 buff 压缩并写到 z 对象的 b 中
	z.Close()
	return base64.StdEncoding.EncodeToString(b.Bytes()) // 加密编码
}

// 压缩并加密字符串
func encodeString(str string) string {
	return encodeBytes([]byte(str))
}

// 解压并解密字符串
func decodeString(str string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(str) // 先解编码
	if err != nil {
		return nil, err
	}
	z, err := zlib.NewReader(bytes.NewReader(b)) // 读取的时候解压
	if err != nil {
		return nil, err
	}
	defer z.Close()
	buf := bytes.NewBuffer(make([]byte, 0, len(b)<<2))
	if _, err := io.Copy(buf, z); err != nil { // copy 到 buf 中
		return nil, err
	}
	return buf.Bytes(), nil
}

type trzszError struct {
	message string
	errType string
	trace   bool
}

var (
	errStopped            = simpleTrzszError("Stopped")
	errStoppedAndDeleted  = simpleTrzszError("Stopped and deleted")
	errReceiveDataTimeout = simpleTrzszError("Receive data timeout")
)

// 创建一个 trzszError 类型
func newTrzszError(message string, errType string, trace bool) *trzszError {
	if errType == "fail" || errType == "FAIL" || errType == "EXIT" {
		msg, err := decodeString(message)
		if err != nil {
			message = fmt.Sprintf("decode [%s] error: %s", message, err)
		} else {
			message = string(msg)
		}
	} else if len(errType) > 0 {
		message = fmt.Sprintf("[TrzszError] %s: %s", errType, message)
	}
	err := &trzszError{message, errType, trace}
	if err.isTraceBack() {
		err.message = fmt.Sprintf("%s\n%s", err.message, string(debug.Stack())) // 是否要跟踪错误
	}
	return err
}

// 创建一个简单 trzszError 类型, 不跟踪错误
func simpleTrzszError(format string, a ...any) *trzszError {
	return newTrzszError(fmt.Sprintf(format, a...), "", false)
}

func (e *trzszError) Error() string {
	return e.message
}

func (e *trzszError) isTraceBack() bool {
	// fail 或 exit 不跟踪错误
	if e.errType == "fail" || e.errType == "EXIT" {
		return false
	}
	return e.trace
}

func (e *trzszError) isRemoteExit() bool {
	return e.errType == "EXIT"
}

func (e *trzszError) isRemoteFail() bool {
	return e.errType == "fail" || e.errType == "FAIL"
}

func (e *trzszError) isStopAndDelete() bool {
	if e == nil || e.errType != "fail" {
		return false
	}
	return e.message == errStoppedAndDeleted.message
}

// 检查文件路径是否可写
func checkPathWritable(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return simpleTrzszError("No such directory: %s", path)
	} else if err != nil {
		return err
	}
	if !info.IsDir() {
		return simpleTrzszError("Not a directory: %s", path)
	}
	if syscallAccessWok(path) != nil {
		return simpleTrzszError("No permission to write: %s", path)
	}
	return nil
}

// json:"-" 表示json不解析该字段
type sourceFile struct {
	PathID   int           `json:"path_id"`
	AbsPath  string        `json:"-"`
	RelPath  []string      `json:"path_name"` // 目录下的所有文件和子目录的相对路径(一个相对路径可以Join成一个绝对路径)
	IsDir    bool          `json:"is_dir"`
	Archive  bool          `json:"archive"`
	Size     int64         `json:"size"`
	Header   string        `json:"-"`
	SubFiles []*sourceFile `json:"-"`
}

func (f *sourceFile) getFileName() string {
	if len(f.RelPath) == 0 {
		return ""
	}
	return f.RelPath[len(f.RelPath)-1]
}

// 从 sourceFile 解析成 json
func (f *sourceFile) marshalSourceFile() (string, error) {
	f.Archive = len(f.SubFiles) > 0
	jstr, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	return string(jstr), nil
}

// 从 json 解析成 sourceFile
func unmarshalSourceFile(source string) (*sourceFile, error) {
	var file sourceFile
	if err := json.Unmarshal([]byte(source), &file); err != nil {
		return nil, err
	}
	if len(file.RelPath) < 1 {
		return nil, simpleTrzszError("Invalid source file: %s", source)
	}
	return &file, nil
}

type targetFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// 从 targetFile 解析成 json
func (f *targetFile) marshalTargetFile() (string, error) {
	jstr, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	return string(jstr), nil
}

// 从 json 解析成 targetFile
func unmarshalTargetFile(target string) (*targetFile, error) {
	var file targetFile
	if err := json.Unmarshal([]byte(target), &file); err != nil {
		return nil, err
	}
	if file.Size < 0 {
		return nil, simpleTrzszError("Invalid target file: %s", target)
	}
	return &file, nil
}

// 检查路径下的所有文件是否可读
func checkPathReadable(pathID int, path string, info os.FileInfo, list *[]*sourceFile,
	relPath []string, visitedDir map[string]bool) error {
	if !info.IsDir() {
		// 如果不是目录(说明是 文件)
		if !info.Mode().IsRegular() {
			return simpleTrzszError("Not a regular file: %s", path)
		}
		if syscallAccessRok(path) != nil {
			return simpleTrzszError("No permission to read: %s", path)
		}
		*list = append(*list, &sourceFile{PathID: pathID, AbsPath: path, RelPath: relPath, Size: info.Size()}) // 添加到 list 中
		return nil
	}
	// 如果是目录
	// 解析符号链接的实际路径
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if _, ok := visitedDir[realPath]; ok {
		return simpleTrzszError("Duplicate link: %s", path)
	}
	visitedDir[realPath] = true
	*list = append(*list, &sourceFile{PathID: pathID, AbsPath: path, RelPath: relPath, IsDir: true})
	fileObj, err := os.Open(path)
	if err != nil {
		return simpleTrzszError("Open [%s] error: %v", path, err)
	}
	files, err := fileObj.Readdir(-1) // 读取目录下的所有文件和子目录返回 []fs.FileInfo
	if err != nil {
		return simpleTrzszError("Readdir [%s] error: %v", path, err)
	}
	for _, file := range files {
		// 将任意数量的指定路径元素连接到单个路径中，并在必要时添加分隔符。此函数对结果调用Clean，所有空字符串都将被忽略。
		// 即 path/file.Name() 拼接
		p := filepath.Join(path, file.Name())
		info, err := os.Stat(p)
		if err != nil {
			return simpleTrzszError("Stat [%s] error: %v", p, err)
		}
		r := make([]string, len(relPath)) // 多复制一份相对路径 slice
		copy(r, relPath)
		r = append(r, file.Name())
		// 递归检查是否可读, 直到该文件路径是一个文件
		if err := checkPathReadable(pathID, p, info, list, r, visitedDir); err != nil {
			return err
		}
	}
	return nil
}

// 检查 一组 paths 是否可读
func checkPathsReadable(paths []string, directory bool) ([]*sourceFile, error) {
	var list []*sourceFile
	for i, p := range paths {
		path, err := filepath.Abs(p) // 返回绝对路径, 如果本身不是绝对路径以当前路径拼接
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return nil, simpleTrzszError("No such file: %s", path)
		} else if err != nil {
			return nil, err
		}
		if !directory && info.IsDir() {
			return nil, simpleTrzszError("Is a directory: %s", path)
		}
		visitedDir := make(map[string]bool)
		if err := checkPathReadable(i, path, info, &list, []string{info.Name()}, visitedDir); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// 检查是否有重复的绝对路径名字
func checkDuplicateNames(sourceFiles []*sourceFile) error {
	m := make(map[string]bool)
	for _, srcFile := range sourceFiles {
		p := filepath.Join(srcFile.RelPath...)
		if _, ok := m[p]; ok {
			return simpleTrzszError("Duplicate name: %s", p)
		}
		m[p] = true
	}
	return nil
}

// 返回一个新的不存在的名字,新名字是 name.i 的形式
func getNewName(path, name string) (string, error) {
	if _, err := os.Stat(filepath.Join(path, name)); os.IsNotExist(err) {
		return name, nil
	}
	for i := 0; i < 1000; i++ {
		newName := fmt.Sprintf("%s.%d", name, i)
		if _, err := os.Stat(filepath.Join(path, newName)); os.IsNotExist(err) {
			return newName, nil
		}
	}
	return "", simpleTrzszError("Fail to assign new file name")
}

type tmuxModeType int

const (
	noTmuxMode = iota
	tmuxNormalMode
	tmuxControlMode
)

// 检查当前的 tmux
func checkTmux() (tmuxModeType, *os.File, int32, error) {
	if _, tmux := os.LookupEnv("TMUX"); !tmux {
		return noTmuxMode, os.Stdout, -1, nil
	}
	// tmux display-message -p '#{client_tty}':'#{client_control_mode}':'#{pane_width}'
	cmd := exec.Command("tmux", "display-message", "-p", "#{client_tty}:#{client_control_mode}:#{pane_width}")
	out, err := cmd.Output()
	if err != nil {
		return 0, nil, -1, fmt.Errorf("get tmux output failed: %v", err)
	}

	output := strings.TrimSpace(string(out))
	tokens := strings.Split(output, ":")
	if len(tokens) != 3 {
		return 0, nil, -1, fmt.Errorf("unexpect tmux output: %s", output)
	}
	tmuxTty, controlMode, paneWidth := tokens[0], tokens[1], tokens[2]

	if controlMode == "1" || tmuxTty[0] != '/' {
		return tmuxControlMode, os.Stdout, -1, nil
	}
	if _, err := os.Stat(tmuxTty); os.IsNotExist(err) {
		return tmuxControlMode, os.Stdout, -1, nil
	}

	tmuxStdout, err := os.OpenFile(tmuxTty, os.O_WRONLY, 0)
	if err != nil {
		return 0, nil, -1, fmt.Errorf("open tmux tty [%s] failed: %v", tmuxTty, err)
	}
	tmuxPaneWidth := -1
	if len(paneWidth) > 0 {
		tmuxPaneWidth, err = strconv.Atoi(paneWidth)
		if err != nil {
			return 0, nil, -1, fmt.Errorf("parse tmux pane width [%s] failed: %v", paneWidth, err)
		}
	}

	statusInterval := getTmuxStatusInterval()
	setTmuxStatusInterval("0")
	onExitFuncs = append(onExitFuncs, func() {
		setTmuxStatusInterval(statusInterval)
	})

	return tmuxNormalMode, tmuxStdout, int32(tmuxPaneWidth), nil
}

func tmuxRefreshClient() {
	cmd := exec.Command("tmux", "refresh-client")
	cmd.Stdout = os.Stdout
	_ = cmd.Run()
}

func getTmuxStatusInterval() string {
	cmd := exec.Command("tmux", "display-message", "-p", "#{status-interval}")
	out, err := cmd.Output()
	output := strings.TrimSpace(string(out))
	if err != nil || output == "" {
		return "15" // The default is 15 seconds
	}
	return output
}

func setTmuxStatusInterval(interval string) {
	if interval == "" {
		interval = "15" // The default is 15 seconds
	}
	cmd := exec.Command("tmux", "setw", "status-interval", interval)
	_ = cmd.Run()
}

// 返回当前终端的 列数
func getTerminalColumns() int {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	output := strings.TrimSpace(string(out))
	tokens := strings.Split(output, " ")
	if len(tokens) != 2 {
		return 0
	}
	cols, _ := strconv.Atoi(tokens[1])
	return cols
}

// 包装 标准输入和 server 端输入
func wrapStdinInput(transfer *trzszTransfer) {
	const bufSize = 32 * 1024 // 32kb
	buffer := make([]byte, bufSize)
	for {
		n, err := os.Stdin.Read(buffer)
		if n > 0 {
			buf := buffer[0:n]
			transfer.addReceivedData(buf)
			buffer = make([]byte, bufSize) // 重新置空
		}
		if err == io.EOF {
			transfer.stopTransferringFiles(false)
		}
	}
}

// 监听退出信号
func handleServerSignal(transfer *trzszTransfer) {
	sigstop := make(chan os.Signal, 1)
	signal.Notify(sigstop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigstop
		transfer.stopTransferringFiles(false)
	}()
}

// 是否是 a-z 或 A-Z (VT100 终端命令的终止字符)
func isVT100End(b byte) bool {
	if 'a' <= b && b <= 'z' {
		return true
	}
	if 'A' <= b && b <= 'Z' {
		return true
	}
	return false
}

// 修剪 字符序列(跳过 VT100 命令)
func trimVT100(buf []byte) []byte {
	b := new(bytes.Buffer)
	skipVT100 := false
	for _, c := range buf {
		if skipVT100 {
			if isVT100End(c) {
				skipVT100 = false
			}
		} else if c == '\x1b' { // '\x1b' == 27 即 esc
			skipVT100 = true
		} else {
			b.WriteByte(c)
		}
	}
	return b.Bytes()
}

// elem 是否包含 v
func containsString(elems []string, v string) bool {
	for _, s := range elems {
		if v == s {
			return true
		}
	}
	return false
}

// 把 data 的字节序列写到 dst
func writeAll(dst io.Writer, data []byte) error {
	m := 0
	l := len(data)
	for m < l {
		n, err := dst.Write(data[m:])
		if err != nil {
			return newTrzszError(fmt.Sprintf("WriteAll error: %v", err), "", true)
		}
		m += n
	}
	return nil
}

type trzszVersion [3]uint32

func parseTrzszVersion(ver string) (*trzszVersion, error) {
	tokens := strings.Split(ver, ".")
	if len(tokens) != 3 {
		return nil, simpleTrzszError("Version [%s] invalid", ver)
	}
	var version trzszVersion
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseUint(tokens[i], 10, 32)
		if err != nil {
			return nil, simpleTrzszError("Version [%s] invalid", ver)
		}
		version[i] = uint32(v)
	}
	return &version, nil
}

// 比较版本
func (v *trzszVersion) compare(ver *trzszVersion) int {
	for i := 0; i < 3; i++ {
		if v[i] < ver[i] {
			return -1
		}
		if v[i] > ver[i] {
			return 1
		}
	}
	return 0
}

type trzszDetector struct {
	relay       bool
	tmux        bool
	uniqueIDMap map[string]int
}

func newTrzszDetector(relay, tmux bool) *trzszDetector {
	return &trzszDetector{relay, tmux, make(map[string]int)}
}

var trzszRegexp = regexp.MustCompile(`::TRZSZ:TRANSFER:([SRD]):(\d+\.\d+\.\d+)(:\d+)?`)
var uniqueIDRegexp = regexp.MustCompile(`::TRZSZ:TRANSFER:[SRD]:\d+\.\d+\.\d+:(\d{13}\d*)`)
var tmuxControlModeRegexp = regexp.MustCompile(`((%output %\d+)|(%extended-output %\d+ \d+ :)) .*::TRZSZ:TRANSFER:`)

func (detector *trzszDetector) rewriteTrzszTrigger(buf []byte) []byte {
	for _, match := range uniqueIDRegexp.FindAllSubmatch(buf, -1) {
		if len(match) == 2 {
			uniqueID := match[1]
			if len(uniqueID) >= 13 && bytes.HasSuffix(uniqueID, []byte("00")) {
				newUniqueID := make([]byte, len(uniqueID))
				copy(newUniqueID, uniqueID)
				newUniqueID[len(uniqueID)-2] = '2'
				buf = bytes.ReplaceAll(buf, uniqueID, newUniqueID)
			}
		}
	}
	return buf
}

func (detector *trzszDetector) addRelaySuffix(output []byte, idx int) []byte {
	idx += 20
	if idx >= len(output) {
		return output
	}
	for ; idx < len(output); idx++ {
		c := output[idx]
		if c != ':' && c != '.' && !(c >= '0' && c <= '9') {
			break
		}
	}
	buf := bytes.NewBuffer(make([]byte, 0, len(output)+2))
	buf.Write(output[:idx])
	buf.Write([]byte("#R"))
	buf.Write(output[idx:])
	return buf.Bytes()
}

func (detector *trzszDetector) detectTrzsz(output []byte) ([]byte, *byte, *trzszVersion, bool) {
	if len(output) < 24 {
		return output, nil, nil, false
	}
	idx := bytes.LastIndex(output, []byte("::TRZSZ:TRANSFER:"))
	if idx < 0 {
		return output, nil, nil, false
	}

	if detector.relay && detector.tmux {
		output = detector.rewriteTrzszTrigger(output)
		idx = bytes.LastIndex(output, []byte("::TRZSZ:TRANSFER:"))
	}

	subOutput := output[idx:]
	match := trzszRegexp.FindSubmatch(subOutput)
	if len(match) < 3 {
		return output, nil, nil, false
	}
	if tmuxControlModeRegexp.Match(output) {
		return output, nil, nil, false
	}

	if len(subOutput) > 40 {
		for _, s := range []string{"#CFG:", "Saved", "Cancelled", "Stopped", "Interrupted"} {
			if bytes.Contains(subOutput[40:], []byte(s)) {
				return output, nil, nil, false
			}
		}
	}

	mode := match[1][0]

	serverVersion, err := parseTrzszVersion(string(match[2]))
	if err != nil {
		return output, nil, nil, false
	}

	uniqueID := ""
	if len(match) > 3 {
		uniqueID = string(match[3])
	}
	if len(uniqueID) >= 8 && (isWindowsEnvironment() || !(len(uniqueID) == 14 && strings.HasSuffix(uniqueID, "00"))) {
		if _, ok := detector.uniqueIDMap[uniqueID]; ok {
			return output, nil, nil, false
		}
		if len(detector.uniqueIDMap) > 100 {
			m := make(map[string]int)
			for k, v := range detector.uniqueIDMap {
				if v >= 50 {
					m[k] = v - 50
				}
			}
			detector.uniqueIDMap = m
		}
		detector.uniqueIDMap[uniqueID] = len(detector.uniqueIDMap)
	}

	remoteIsWindows := false
	if uniqueID == ":1" || (len(uniqueID) == 14 && strings.HasSuffix(uniqueID, "10")) {
		remoteIsWindows = true
	}

	if detector.relay {
		output = detector.addRelaySuffix(output, idx)
	} else {
		output = bytes.ReplaceAll(output, []byte("TRZSZ"), []byte("TRZSZGO"))
	}

	return output, &mode, serverVersion, remoteIsWindows
}

type traceLogger struct {
	traceLogFile atomic.Pointer[os.File]
	traceLogChan atomic.Pointer[chan []byte]
}

func newTraceLogger() *traceLogger {
	return &traceLogger{}
}

/**
 * ┌────────────────────┬───────────────────────────────────────────┬────────────────────────────────────────────┐
 * │                    │ Enable trace log                          │ Disable trace log                          │
 * ├────────────────────┼───────────────────────────────────────────┼────────────────────────────────────────────┤
 * │ Windows cmd        │ echo ^<ENABLE_TRZSZ_TRACE_LOG^>           │ echo ^<DISABLE_TRZSZ_TRACE_LOG^>           │
 * ├────────────────────┼───────────────────────────────────────────┼────────────────────────────────────────────┤
 * │ Windows PowerShell │ echo "<ENABLE_TRZSZ_TRACE_LOG$([char]62)" │ echo "<DISABLE_TRZSZ_TRACE_LOG$([char]62)" │
 * ├────────────────────┼───────────────────────────────────────────┼────────────────────────────────────────────┤
 * │ Linux and macOS    │ echo -e '<ENABLE_TRZSZ_TRACE_LOG\x3E'     │ echo -e '<DISABLE_TRZSZ_TRACE_LOG\x3E'     │
 * └────────────────────┴───────────────────────────────────────────┴────────────────────────────────────────────┘
 */

// 把 buf 编码后写到追踪日志中
func (logger *traceLogger) writeTraceLog(buf []byte, typ string) []byte {
	if ch, file := logger.traceLogChan.Load(), logger.traceLogFile.Load(); ch != nil && file != nil {
		if typ == "svrout" && bytes.Contains(buf, []byte("<DISABLE_TRZSZ_TRACE_LOG>")) {
			msg := fmt.Sprintf("Closed trace log at %s", file.Name())
			close(*ch)
			logger.traceLogChan.Store(nil)
			logger.traceLogFile.Store(nil)
			return bytes.ReplaceAll(buf, []byte("<DISABLE_TRZSZ_TRACE_LOG>"), []byte(msg))
		}
		*ch <- []byte(fmt.Sprintf("[%s]%s\n", typ, encodeBytes(buf)))
		return buf
	}
	if typ == "svrout" && bytes.Contains(buf, []byte("<ENABLE_TRZSZ_TRACE_LOG>")) {
		var msg string
		file, err := os.CreateTemp("", "trzsz_*.log")
		if err != nil {
			msg = fmt.Sprintf("Create log file error: %v", err)
		} else {
			msg = fmt.Sprintf("Writing trace log to %s", file.Name())
		}
		ch := make(chan []byte, 10000)
		logger.traceLogChan.Store(&ch)
		logger.traceLogFile.Store(file)
		go func() {
			for {
				select {
				case buf, ok := <-ch:
					if !ok {
						file.Close()
						return
					}
					_ = writeAll(file, buf)
				case <-time.After(3 * time.Second):
					_ = file.Sync()
				}
			}
		}()
		return bytes.ReplaceAll(buf, []byte("<ENABLE_TRZSZ_TRACE_LOG>"), []byte(msg))
	}
	return buf
}

const compressedBlockSize = 128 << 10

func isCompressedFileContent(file *os.File, pos int64) (bool, error) {
	if _, err := file.Seek(pos, io.SeekStart); err != nil {
		return false, err
	}

	buffer := make([]byte, compressedBlockSize)
	_, err := io.ReadFull(file, buffer)
	if err != nil {
		return false, err
	}

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return false, err
	}
	dst := make([]byte, 0, compressedBlockSize+0x20)
	return len(encoder.EncodeAll(buffer, dst)) > compressedBlockSize*98/100, nil
}

func isCompressionProfitable(reader fileReader) (bool, error) {
	file := reader.getFile()
	if file == nil {
		return true, nil
	}
	size := reader.getSize()

	pos, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, err
	}

	compressedCount := 0
	if size >= compressedBlockSize {
		compressed, err := isCompressedFileContent(file, pos)
		if err != nil {
			return false, err
		}
		if compressed {
			compressedCount++
		}
	}

	if size >= 2*compressedBlockSize {
		compressed, err := isCompressedFileContent(file, pos+size-compressedBlockSize)
		if err != nil {
			return false, err
		}
		if compressed {
			compressedCount++
		}
	}

	if size >= 3*compressedBlockSize {
		compressed, err := isCompressedFileContent(file, pos+(size/2)-(compressedBlockSize/2))
		if err != nil {
			return false, err
		}
		if compressed {
			compressedCount++
		}
	}

	if _, err := file.Seek(pos, io.SeekStart); err != nil {
		return false, err
	}

	return compressedCount < 2, nil
}

func formatSavedFiles(fileNames []string, destPath string) string {
	var builder strings.Builder
	builder.WriteString("Saved ")
	builder.WriteString(strconv.Itoa(len(fileNames)))
	if len(fileNames) > 1 {
		builder.WriteString(" files/directories")
	} else {
		builder.WriteString(" file/directory")
	}
	if len(destPath) > 0 {
		builder.WriteString(" to ")
		builder.WriteString(destPath)
	}
	for _, name := range fileNames {
		builder.WriteString("\r\n- ")
		builder.WriteString(name)
	}
	return builder.String()
}

func joinFileNames(header string, fileNames []string) string {
	var builder strings.Builder
	builder.WriteString(header)
	for _, name := range fileNames {
		builder.WriteString("\r\n- ")
		builder.WriteString(name)
	}
	return builder.String()
}
