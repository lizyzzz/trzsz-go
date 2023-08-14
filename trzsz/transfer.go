/*
MIT License

Copyright (c) 2023 Lonny Wong <lonnywong@qq.com>

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
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	kProtocolVersion2 = 2
	kProtocolVersion3 = 3
	kProtocolVersion4 = 4
	kProtocolVersion  = kProtocolVersion4

	kLastChunkTimeCount = 10
)

type transferAction struct {
	Lang             string `json:"lang"`
	Version          string `json:"version"` // 版本号
	Confirm          bool   `json:"confirm"`
	Newline          string `json:"newline"`     // 新一行的标记
	Protocol         int    `json:"protocol"`    // 协议
	SupportBinary    bool   `json:"binary"`      // 是否支持二进制序列
	SupportDirectory bool   `json:"support_dir"` // 是否支持目录
}

type transferConfig struct {
	Quiet           bool         `json:"quiet"`            // 是否隐藏进度条
	Binary          bool         `json:"binary"`           // 是否传输二进制文件(字节序列)
	Directory       bool         `json:"directory"`        // 是否传输目录
	Overwrite       bool         `json:"overwrite"`        // 是否覆盖已有文件
	Timeout         int          `json:"timeout"`          // 超时时间
	Newline         string       `json:"newline"`          // 新一行的标记
	Protocol        int          `json:"protocol"`         // 传输协议
	MaxBufSize      int64        `json:"bufsize"`          // 最大缓冲空间
	EscapeCodes     escapeArray  `json:"escape_chars"`     // 转义表
	TmuxPaneColumns int32        `json:"tmux_pane_width"`  // Tmux 列数
	TmuxOutputJunk  bool         `json:"tmux_output_junk"` // tmux 是否会输出 \r 字符
	CompressType    compressType `json:"compress"`         // 压缩类型
}

type trzszTransfer struct {
	buffer           *trzszBuffer                       // 数据接收的 buffer(read)
	writer           io.Writer                          // 数据发送的 write
	stopped          atomic.Bool                        // 是否停止
	stopAndDelete    atomic.Bool                        // 是否停止并删除
	pausing          atomic.Bool                        // 是否正在暂停
	pauseIdx         atomic.Uint32                      // 暂停的次数
	pauseBeginTime   atomic.Int64                       // 暂停的开始时间
	resumeBeginTime  atomic.Pointer[time.Time]          // 恢复传输的时间
	lastInputTime    atomic.Int64                       // 上一次的输入时间
	cleanTimeout     time.Duration                      // 清空输入的超时时间
	lastChunkTimeArr [kLastChunkTimeCount]time.Duration // 最近 k 块的
	lastChunkTimeIdx int                                // 最后一块的时间下标
	stdinState       *term.State                        // stdin 的原始状态
	fileNameMap      map[int]string
	windowsProtocol  bool
	flushInTime      bool // 是否及时刷盘
	bufInitWG        sync.WaitGroup
	bufInitPhase     atomic.Bool
	bufferSize       atomic.Int64
	savedSteps       atomic.Int64
	transferConfig   transferConfig
	logger           *traceLogger
	createdFiles     []string // 已经创建的文件
}

// 两个最大的 time.Duration
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// 两个最小的 int64
func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// 两个最小的 int
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 创建一个新的 transfer 对象
func newTransfer(writer io.Writer, stdinState *term.State, flushInTime bool, logger *traceLogger) *trzszTransfer {
	t := &trzszTransfer{
		buffer:       newTrzszBuffer(),
		writer:       writer,
		cleanTimeout: 100 * time.Millisecond,
		stdinState:   stdinState,
		fileNameMap:  make(map[int]string),
		flushInTime:  flushInTime,
		transferConfig: transferConfig{
			Timeout:    20,
			Newline:    "\n",
			MaxBufSize: 10 * 1024 * 1024,
		},
		logger: logger,
	}
	t.bufInitPhase.Store(true)
	t.bufferSize.Store(10240)
	return t
}

// 添加接收到的数据
func (t *trzszTransfer) addReceivedData(buf []byte) {
	if !t.stopped.Load() {
		t.buffer.addBuffer(buf)
	}
	t.lastInputTime.Store(time.Now().UnixMilli())
}

// 停止正在传输的文件
func (t *trzszTransfer) stopTransferringFiles(stopAndDelete bool) {
	if t.stopped.Load() {
		return
	}
	t.stopAndDelete.Store(stopAndDelete)
	t.stopped.Store(true)
	t.buffer.stopBuffer()

	// 更新清空输入的超时时间
	maxChunkTime := time.Duration(0)
	for _, chunkTime := range t.lastChunkTimeArr {
		if chunkTime > maxChunkTime {
			maxChunkTime = chunkTime
		}
	}
	waitTime := maxChunkTime * 2
	beginTime := t.pauseBeginTime.Load()
	if beginTime > 0 {
		waitTime -= time.Since(time.UnixMilli(beginTime))
	}
	t.cleanTimeout = maxDuration(waitTime, 500*time.Millisecond)
}

// 暂停正在传输的文件
func (t *trzszTransfer) pauseTransferringFiles() {
	t.pausing.Store(true)
	if t.pauseBeginTime.Load() == 0 {
		t.pauseIdx.Add(1)
		t.pauseBeginTime.CompareAndSwap(0, time.Now().UnixMilli())
	}
}

// 恢复传输文件
func (t *trzszTransfer) resumeTransferringFiles() {
	now := timeNowFunc()
	t.resumeBeginTime.Store(&now)
	t.pauseBeginTime.Store(0)
	t.buffer.setNewTimeout(t.getNewTimeout())
	t.pausing.Store(false)
}

// 检查是否已经 stop , yes 则 err != nil, no 则 err == nil
func (t *trzszTransfer) checkStop() error {
	if t.stopAndDelete.Load() {
		return errStoppedAndDeleted
	}
	if t.stopped.Load() {
		return errStopped
	}
	return nil
}

// 设置最后一块的时间
func (t *trzszTransfer) setLastChunkTime(chunkTime time.Duration) {
	// 循环存放在 lastChunkTime 中, 并更新 idx
	t.lastChunkTimeArr[t.lastChunkTimeIdx] = chunkTime
	t.lastChunkTimeIdx++
	if t.lastChunkTimeIdx >= kLastChunkTimeCount {
		t.lastChunkTimeIdx = 0
	}
}

// 清除输入 (并阻塞 指定时间)
func (t *trzszTransfer) cleanInput(timeoutDuration time.Duration) {
	t.stopped.Store(true)
	t.buffer.drainBuffer()
	t.lastInputTime.Store(time.Now().UnixMilli())
	for {
		sleepDuration := timeoutDuration - time.Since(time.UnixMilli(t.lastInputTime.Load()))
		if sleepDuration <= 0 {
			return
		}
		time.Sleep(sleepDuration)
	}
}

// 把 buf 全部写到 writer
func (t *trzszTransfer) writeAll(buf []byte) error {
	if t.logger != nil {
		t.logger.writeTraceLog(buf, "tosvr")
	}
	return writeAll(t.writer, buf)
}

// 发送一行数据: 头部是 typ, 中间是 buf, 尾部是换行符 ("#typ:buf\n") 其中 \n 是换行符可以由config指定
func (t *trzszTransfer) sendLine(typ string, buf string) error {
	return t.writeAll([]byte(fmt.Sprintf("#%s:%s%s", typ, buf, t.transferConfig.Newline)))
}

// 清除 tmux 状态行 (保留其他字符串)
func (t *trzszTransfer) stripTmuxStatusLine(buf []byte) []byte {
	for {
		beginIdx := bytes.Index(buf, []byte("\x1bP="))
		if beginIdx < 0 {
			return buf
		}
		bufIdx := beginIdx + 3 // 跳过 \x1bP= [设备控制字符串：代表用户自定义密钥]
		midIdx := bytes.Index(buf[bufIdx:], []byte("\x1bP="))
		if midIdx < 0 {
			return buf[:beginIdx]
		}
		bufIdx += midIdx + 3
		endIdx := bytes.Index(buf[bufIdx:], []byte("\x1b\\")) // 'ESC \' 终止其他控件（包括APC，DCS，OSC，PM和SOS）中的字符串
		if endIdx < 0 {
			return buf[:beginIdx]
		}
		bufIdx += endIdx + 2
		b := bytes.NewBuffer(make([]byte, 0, len(buf)-(bufIdx-beginIdx)))
		b.Write(buf[:beginIdx])
		b.Write(buf[bufIdx:]) // 相当于 去掉了 [beginIdx, bufIdx) 的内容
		buf = b.Bytes()
	}
}

// 接收期望类型的一行 (不保证一定是期望类型) "#expectType:buf"
func (t *trzszTransfer) recvLine(expectType string, mayHasJunk bool, timeout <-chan time.Time) ([]byte, error) {
	if err := t.checkStop(); err != nil {
		return nil, err
	}

	// windows 环境下
	if isWindowsEnvironment() || t.windowsProtocol {
		line, err := t.buffer.readLineOnWindows(timeout)
		if err != nil {
			if e := t.checkStop(); e != nil {
				return nil, e
			}
			return nil, err
		}
		// 读取期望类型的行
		idx := bytes.LastIndex(line, []byte("#"+expectType+":"))
		if idx >= 0 {
			line = line[idx:]
		} else {
			// 读取非期望类型的一行
			idx = bytes.LastIndexByte(line, '#')
			if idx > 0 {
				line = line[idx:]
			}
		}
		return line, nil
	}

	// 非 windows 环境
	line, err := t.buffer.readLine(t.transferConfig.TmuxOutputJunk || mayHasJunk, timeout)
	if err != nil {
		if e := t.checkStop(); e != nil {
			return nil, e
		}
		return nil, err
	}

	if t.transferConfig.TmuxOutputJunk || mayHasJunk {
		idx := bytes.LastIndex(line, []byte("#"+expectType+":"))
		if idx >= 0 {
			line = line[idx:]
		} else {
			idx = bytes.LastIndexByte(line, '#')
			if idx > 0 {
				line = line[idx:]
			}
		}
		line = t.stripTmuxStatusLine(line)
	}

	return line, nil
}

// 接收期望类型的一行 (保证一定是期望类型) "#expectType:buf"
func (t *trzszTransfer) recvCheck(expectType string, mayHasJunk bool, timeout <-chan time.Time) (string, error) {
	line, err := t.recvLine(expectType, mayHasJunk, timeout)
	if err != nil {
		return "", err
	}

	idx := bytes.IndexByte(line, ':')
	if idx < 1 {
		return "", newTrzszError(encodeBytes(line), "colon", true)
	}

	typ := string(line[1:idx])
	buf := string(line[idx+1:])
	if typ != expectType {
		return "", newTrzszError(buf, typ, true)
	}

	return buf, nil
}

// 发送一个 int "#typ:11\n"
func (t *trzszTransfer) sendInteger(typ string, val int64) error {
	return t.sendLine(typ, strconv.FormatInt(val, 10))
}

// 接收一个 int
func (t *trzszTransfer) recvInteger(typ string, mayHasJunk bool, timeout <-chan time.Time) (int64, error) {
	buf, err := t.recvCheck(typ, mayHasJunk, timeout)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(buf, 10, 64)
}

// 接收一个类型是"SUCC"的 int 并比较 期望的 expect (int) 是否相等
func (t *trzszTransfer) checkInteger(expect int64, timeout <-chan time.Time) error {
	result, err := t.recvInteger("SUCC", false, timeout)
	if err != nil {
		return err
	}
	if result != expect {
		return newTrzszError(fmt.Sprintf("Integer check [%d] <> [%d]", result, expect), "", true)
	}
	return nil
}

// 发送一个 string
func (t *trzszTransfer) sendString(typ string, str string) error {
	return t.sendLine(typ, encodeString(str))
}

// 接收一个 string
func (t *trzszTransfer) recvString(typ string, mayHasJunk bool, timeout <-chan time.Time) (string, error) {
	buf, err := t.recvCheck(typ, mayHasJunk, timeout)
	if err != nil {
		return "", err
	}
	b, err := decodeString(buf)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// 接收一个类型是"SUCC"的 string 并比较 期望的 expect (string) 是否相等
func (t *trzszTransfer) checkString(expect string, timeout <-chan time.Time) error { // nolint:all
	result, err := t.recvString("SUCC", false, timeout)
	if err != nil {
		return err
	}
	if result != expect {
		return newTrzszError(fmt.Sprintf("String check [%s] <> [%s]", result, expect), "", true)
	}
	return nil
}

// 发送一个二进制文件(字节序列)
func (t *trzszTransfer) sendBinary(typ string, buf []byte) error {
	return t.sendLine(typ, encodeBytes(buf))
}

// 接收一个二进制文件(字节序列)
func (t *trzszTransfer) recvBinary(typ string, mayHasJunk bool, timeout <-chan time.Time) ([]byte, error) {
	buf, err := t.recvCheck(typ, mayHasJunk, timeout)
	if err != nil {
		return nil, err
	}
	return decodeString(buf)
}

// 接收一个 二进制 字符序列, 并比较 期望的 expect ([]byte) 是否相等
func (t *trzszTransfer) checkBinary(expect []byte, timeout <-chan time.Time) error {
	result, err := t.recvBinary("SUCC", false, timeout)
	if err != nil {
		return err
	}
	if bytes.Compare(result, expect) != 0 { // nolint:all
		return newTrzszError(fmt.Sprintf("Binary check [%v] <> [%v]", result, expect), "", true)
	}
	return nil
}

// 发送一个字节系列
func (t *trzszTransfer) sendData(data []byte) error {
	if err := t.checkStop(); err != nil {
		return err
	}
	if !t.transferConfig.Binary {
		// 如果不是 发送二进制序列
		return t.sendBinary("DATA", data)
	}
	buf := escapeData(data, t.transferConfig.EscapeCodes)
	if err := t.writeAll([]byte(fmt.Sprintf("#DATA:%d\n", len(buf)))); err != nil {
		return err
	}
	return t.writeAll(buf)
}

func (t *trzszTransfer) getNewTimeout() <-chan time.Time {
	if t.transferConfig.Timeout > 0 {
		return time.NewTimer(time.Duration(t.transferConfig.Timeout) * time.Second).C
	}
	return nil
}

func (t *trzszTransfer) recvData() ([]byte, error) {
	timeout := t.getNewTimeout()
	if !t.transferConfig.Binary {
		return t.recvBinary("DATA", false, timeout)
	}
	size, err := t.recvInteger("DATA", false, timeout)
	if err != nil {
		return nil, err
	}
	data, err := t.buffer.readBinary(int(size), timeout)
	if err != nil {
		if e := t.checkStop(); e != nil {
			return nil, e
		}
		return nil, err
	}
	buf, remaining, err := unescapeData(data, t.transferConfig.EscapeCodes, nil)
	if err != nil {
		return nil, err
	}
	if len(remaining) != 0 {
		return nil, simpleTrzszError("Unescape has bytes remaining: %v", remaining)
	}
	return buf, nil
}

// 发送 关于发送操作的配置
func (t *trzszTransfer) sendAction(confirm bool, serverVersion *trzszVersion, remoteIsWindows bool) error {
	protocol := kProtocolVersion
	if serverVersion != nil &&
		serverVersion.compare(&trzszVersion{1, 1, 3}) <= 0 && serverVersion.compare(&trzszVersion{1, 1, 0}) >= 0 {
		protocol = 2 // compatible with older versions
	}
	action := &transferAction{
		Lang:             "go",
		Version:          kTrzszVersion,
		Confirm:          confirm,
		Newline:          "\n",
		Protocol:         protocol,
		SupportBinary:    true,
		SupportDirectory: true,
	}
	if isWindowsEnvironment() || remoteIsWindows {
		action.Newline = "!\n"
		action.SupportBinary = false
	}
	actStr, err := json.Marshal(action) // 解析成 json 格式发送
	if err != nil {
		return err
	}
	if remoteIsWindows {
		t.windowsProtocol = true
		t.transferConfig.Newline = "!\n"
	}
	return t.sendString("ACT", string(actStr)) // 发送 ACT 类型 json
}

// 接收 关于发送操作的配置
func (t *trzszTransfer) recvAction() (*transferAction, error) {
	actStr, err := t.recvString("ACT", false, nil) // 接收 ACT 类型(参数为什么是 false, nil)
	if err != nil {
		return nil, err
	}
	action := &transferAction{
		Newline:       "\n",
		SupportBinary: true,
	}
	if err := json.Unmarshal([]byte(actStr), action); err != nil {
		return nil, err
	}
	t.transferConfig.Newline = action.Newline
	return action, nil
}

// 发送 配置文件
func (t *trzszTransfer) sendConfig(args *baseArgs, action *transferAction, escapeChars [][]unicode, tmuxMode tmuxModeType, tmuxPaneWidth int32) error {
	cfgMap := map[string]interface{}{
		"lang": "go",
	}
	if args.Quiet {
		cfgMap["quiet"] = true
	}
	if args.Binary {
		// 二进制模式下需要转义
		cfgMap["binary"] = true
		cfgMap["escape_chars"] = escapeChars
	}
	if args.Directory {
		cfgMap["directory"] = true
	}
	cfgMap["bufsize"] = args.Bufsize.Size
	cfgMap["timeout"] = args.Timeout
	if args.Overwrite {
		cfgMap["overwrite"] = true
	}
	if tmuxMode == tmuxNormalMode {
		cfgMap["tmux_output_junk"] = true
		cfgMap["tmux_pane_width"] = tmuxPaneWidth
	}
	if action.Protocol > 0 {
		cfgMap["protocol"] = minInt(action.Protocol, kProtocolVersion)
	}
	if args.Compress != kCompressAuto {
		cfgMap["compress"] = args.Compress
	}
	cfgStr, err := json.Marshal(cfgMap) // 解析到 json
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(cfgStr), &t.transferConfig); err != nil {
		// 从 json 解析到 transferConfig
		return err
	}
	return t.sendString("CFG", string(cfgStr)) // 发送 cfg 类型
}

// 接收配置文件
func (t *trzszTransfer) recvConfig() (*transferConfig, error) {
	cfgStr, err := t.recvString("CFG", true, t.getNewTimeout()) // 接收 CFG 类型(参数为什么是 true, newTimeout)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(cfgStr), &t.transferConfig); err != nil {
		return nil, err
	}
	return &t.transferConfig, nil
}

// 客户端退出, 发送退出消息
func (t *trzszTransfer) clientExit(msg string) error {
	return t.sendString("EXIT", msg)
}

// 接收客户端退出消息
func (t *trzszTransfer) recvExit() (string, error) {
	return t.recvString("EXIT", false, t.getNewTimeout())
}

// 服务端退出
func (t *trzszTransfer) serverExit(msg string) {
	t.cleanInput(500 * time.Millisecond) // 首先清除输入
	if t.stdinState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), t.stdinState) // 恢复原来的状态 (t.stdinState记录了原来的状态)
	}
	if isRunningOnWindows() {
		msg = strings.ReplaceAll(msg, "\n", "\r\n")       // windows 下把 \n 替换成 \r\n
		os.Stdout.WriteString("\x1b[H\x1b[2J\x1b[?1049l") // \x1b[H - 光标回到 1,1 位置; \x1b[2J - 清屏
	} else {
		os.Stdout.WriteString("\x1b8\x1b[0J") // \x1b[0J - 清除光标之后的内容
	}
	os.Stdout.WriteString(msg)    // 写到 标准输出
	os.Stdout.WriteString("\r\n") // 换行
	if t.transferConfig.TmuxOutputJunk {
		tmuxRefreshClient() // 更新 tmux 客户端
	}
}

// 删除已创建的文件, 返回已删除的文件
func (t *trzszTransfer) deleteCreatedFiles() []string {
	var deletedFiles []string
	for _, path := range t.createdFiles {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(path); err == nil {
			// 删除 path 路径下的所有内容
			deletedFiles = append(deletedFiles, path)
		}
	}
	return deletedFiles
}

// 处理客户端的错误
func (t *trzszTransfer) clientError(err error) {
	t.cleanInput(t.cleanTimeout)

	trace := true
	if e, ok := err.(*trzszError); ok {
		trace = e.isTraceBack()
		if e.isRemoteExit() || e.isRemoteFail() {
			return
		}
	}

	if t.stopAndDelete.Load() {
		// 如果传输错误需要停止并删除
		deletedFiles := t.deleteCreatedFiles()
		if len(deletedFiles) > 0 {
			_ = t.sendString("fail", joinFileNames(err.Error()+":", deletedFiles)) // 发送传输失败的文件路径
			return
		}
	}

	typ := "fail"
	if trace {
		typ = "FAIL"
	}
	_ = t.sendString(typ, err.Error()) // 发送错误信息
}

// 处理服务端错误
func (t *trzszTransfer) serverError(err error) {
	t.cleanInput(t.cleanTimeout)

	// 服务端错误先由 err 判断
	trace := true
	if e, ok := err.(*trzszError); ok {
		if e.isStopAndDelete() {
			deletedFiles := t.deleteCreatedFiles()
			if len(deletedFiles) > 0 {
				t.serverExit(joinFileNames(err.Error()+":", deletedFiles))
				return
			}
		}
		trace = e.isTraceBack()
		if e.isRemoteExit() || e.isRemoteFail() {
			t.serverExit(e.Error())
			return
		}
	}

	typ := "fail"
	if trace {
		typ = "FAIL"
	}
	_ = t.sendString(typ, err.Error())

	t.serverExit(err.Error())
}

// 发送文件 num
func (t *trzszTransfer) sendFileNum(num int64, progress progressCallback) error {
	if err := t.sendInteger("NUM", num); err != nil {
		return err
	}
	// 接收 响应 "SUCC"
	if err := t.checkInteger(num, t.getNewTimeout()); err != nil {
		return err
	}
	if progress != nil {
		progress.onNum(num)
	}
	return nil
}

// 发送文件名称
func (t *trzszTransfer) sendFileName(srcFile *sourceFile, progress progressCallback) (fileReader, string, error) {
	var fileName string
	// 如果是目录先解析成 json
	if t.transferConfig.Directory {
		jsonName, err := srcFile.marshalSourceFile()
		if err != nil {
			return nil, "", err
		}
		fileName = jsonName
	} else {
		fileName = srcFile.getFileName()
	}
	// 发送文件名称
	if err := t.sendString("NAME", fileName); err != nil {
		return nil, "", err
	}
	// 接收文件名称的确认
	remoteName, err := t.recvString("SUCC", false, t.getNewTimeout())
	if err != nil {
		return nil, "", err
	}

	if progress != nil {
		progress.onName(srcFile.getFileName())
	}

	if srcFile.IsDir {
		return nil, remoteName, nil
	}

	file, err := os.Open(srcFile.AbsPath)
	if err != nil {
		return nil, "", err
	}

	return &simpleFileReader{file, srcFile.Size}, remoteName, nil
}

func (t *trzszTransfer) sendFileSize(size int64, progress progressCallback) error {
	if err := t.sendInteger("SIZE", size); err != nil {
		return err
	}
	if err := t.checkInteger(size, t.getNewTimeout()); err != nil {
		return err
	}
	if progress != nil {
		progress.onSize(size)
	}
	return nil
}

func (t *trzszTransfer) sendFileData(file fileReader, progress progressCallback) ([]byte, error) {
	step := int64(0)
	if progress != nil {
		progress.onStep(step)
	}
	bufSize := int64(1024)
	buffer := make([]byte, bufSize)
	hasher := md5.New()
	size := file.getSize()
	for step < size {
		beginTime := time.Now()
		m := size - step
		if m < bufSize {
			buffer = buffer[:m]
		}
		n, err := file.Read(buffer)
		length := int64(n)
		if err == io.EOF {
			if length+step != size {
				return nil, simpleTrzszError("EOF but length [%d] + step [%d] <> size [%d]", length, step, size)
			}
		} else if err != nil {
			return nil, err
		}
		data := buffer[:n]
		if err := t.sendData(data); err != nil {
			return nil, err
		}
		if _, err := hasher.Write(data); err != nil {
			return nil, err
		}
		if err := t.checkInteger(length, t.getNewTimeout()); err != nil {
			return nil, err
		}
		step += length
		if progress != nil {
			progress.onStep(step)
		}
		chunkTime := time.Since(beginTime)
		if length == bufSize && chunkTime < 500*time.Millisecond && bufSize < t.transferConfig.MaxBufSize {
			bufSize = minInt64(bufSize*2, t.transferConfig.MaxBufSize)
			buffer = make([]byte, bufSize)
		} else if chunkTime >= 2*time.Second && bufSize > 1024 {
			bufSize = 1024
			buffer = make([]byte, bufSize)
		}
		t.setLastChunkTime(chunkTime)
	}
	return hasher.Sum(nil), nil
}

func (t *trzszTransfer) sendFileMD5(digest []byte, progress progressCallback) error {
	if err := t.sendBinary("MD5", digest); err != nil {
		return err
	}
	if err := t.checkBinary(digest, t.getNewTimeout()); err != nil {
		return err
	}
	if progress != nil {
		progress.onDone()
	}
	return nil
}

func (t *trzszTransfer) sendFiles(sourceFiles []*sourceFile, progress progressCallback) ([]string, error) {
	sourceFiles = t.archiveSourceFiles(sourceFiles)
	if err := t.sendFileNum(int64(len(sourceFiles)), progress); err != nil {
		return nil, err
	}

	var remoteNames []string
	for _, srcFile := range sourceFiles {
		var err error
		var file fileReader
		var remoteName string
		if t.transferConfig.Protocol >= kProtocolVersion3 {
			file, remoteName, err = t.sendFileNameV3(srcFile, progress)
		} else {
			file, remoteName, err = t.sendFileName(srcFile, progress)
		}
		if err != nil {
			return nil, err
		}

		if !containsString(remoteNames, remoteName) {
			remoteNames = append(remoteNames, remoteName)
		}

		if file == nil {
			continue
		}

		defer file.Close()

		if err := t.sendFileSize(file.getSize(), progress); err != nil {
			return nil, err
		}

		var digest []byte
		if t.transferConfig.Protocol >= kProtocolVersion2 {
			digest, err = t.sendFileDataV2(file, progress)
		} else {
			digest, err = t.sendFileData(file, progress)
		}
		if err != nil {
			return nil, err
		}

		if err := t.sendFileMD5(digest, progress); err != nil {
			return nil, err
		}
	}

	return remoteNames, nil
}

func (t *trzszTransfer) recvFileNum(progress progressCallback) (int64, error) {
	num, err := t.recvInteger("NUM", false, t.getNewTimeout())
	if err != nil {
		return 0, err
	}
	if err := t.sendInteger("SUCC", num); err != nil {
		return 0, err
	}
	if progress != nil {
		progress.onNum(num)
	}
	return num, nil
}

func (t *trzszTransfer) addCreatedFiles(path string) {
	t.createdFiles = append(t.createdFiles, path)
}

func (t *trzszTransfer) doCreateFile(path string, truncate bool) (fileWriter, error) {
	flag := os.O_RDWR | os.O_CREATE
	if truncate {
		flag |= os.O_TRUNC
	}
	file, err := os.OpenFile(path, flag, 0644)
	if err != nil {
		if e, ok := err.(*fs.PathError); ok {
			if errno, ok := e.Unwrap().(syscall.Errno); ok {
				if (!isRunningOnWindows() && errno == 13) || (isRunningOnWindows() && errno == 5) {
					return nil, simpleTrzszError("No permission to write: %s", path)
				} else if (!isRunningOnWindows() && errno == 21) || (isRunningOnWindows() && errno == 0x2000002a) {
					return nil, simpleTrzszError("Is a directory: %s", path)
				}
			}
		}
		return nil, simpleTrzszError("Create file [%s] failed: %v", path, err)
	}
	t.addCreatedFiles(path)
	return &simpleFileWriter{file}, nil
}

func (t *trzszTransfer) doCreateDirectory(path string) error {
	stat, err := os.Stat(path)
	if os.IsNotExist(err) {
		err := os.MkdirAll(path, 0755)
		if err != nil {
			return err
		}
		t.addCreatedFiles(path)
		return nil
	} else if err != nil {
		return err
	}
	if !stat.IsDir() {
		return simpleTrzszError("Not a directory: %s", path)
	}
	return nil
}

func (t *trzszTransfer) createFile(path, fileName string, truncate bool) (fileWriter, string, error) {
	var localName string
	if t.transferConfig.Overwrite {
		localName = fileName
	} else {
		var err error
		localName, err = getNewName(path, fileName)
		if err != nil {
			return nil, "", err
		}
	}
	file, err := t.doCreateFile(filepath.Join(path, localName), truncate)
	if err != nil {
		return nil, "", err
	}
	return file, localName, nil
}

func (t *trzszTransfer) createDirOrFile(path string, srcFile *sourceFile, truncate bool) (fileWriter, string, error) {
	var localName string
	if t.transferConfig.Overwrite {
		localName = srcFile.RelPath[0]
	} else {
		if v, ok := t.fileNameMap[srcFile.PathID]; ok {
			localName = v
		} else {
			var err error
			localName, err = getNewName(path, srcFile.RelPath[0])
			if err != nil {
				return nil, "", err
			}
			t.fileNameMap[srcFile.PathID] = localName
		}
	}

	var fullPath string
	if len(srcFile.RelPath) > 1 {
		p := filepath.Join(append([]string{path, localName}, srcFile.RelPath[1:len(srcFile.RelPath)-1]...)...)
		if err := t.doCreateDirectory(p); err != nil {
			return nil, "", err
		}
		fullPath = filepath.Join(p, srcFile.getFileName())
	} else {
		fullPath = filepath.Join(path, localName)
	}

	if srcFile.Archive {
		file, err := t.newArchiveWriter(path, srcFile, fullPath)
		if err != nil {
			return nil, "", err
		}
		return file, localName, nil
	}

	if srcFile.IsDir {
		if err := t.doCreateDirectory(fullPath); err != nil {
			return nil, "", err
		}
		return nil, localName, nil
	}

	file, err := t.doCreateFile(fullPath, truncate)
	if err != nil {
		return nil, "", err
	}
	return file, localName, nil
}

func (t *trzszTransfer) recvFileName(path string, progress progressCallback) (fileWriter, string, error) {
	fileName, err := t.recvString("NAME", false, t.getNewTimeout())
	if err != nil {
		return nil, "", err
	}

	var file fileWriter
	var localName string
	if t.transferConfig.Directory {
		var srcFile *sourceFile
		srcFile, err = unmarshalSourceFile(fileName)
		if err != nil {
			return nil, "", err
		}
		fileName = srcFile.getFileName()
		file, localName, err = t.createDirOrFile(path, srcFile, true)
	} else {
		file, localName, err = t.createFile(path, fileName, true)
	}
	if err != nil {
		return nil, "", err
	}

	if err := t.sendString("SUCC", localName); err != nil {
		return nil, "", err
	}
	if progress != nil {
		progress.onName(fileName)
	}

	return file, localName, nil
}

func (t *trzszTransfer) recvFileSize(progress progressCallback) (int64, error) {
	size, err := t.recvInteger("SIZE", false, t.getNewTimeout())
	if err != nil {
		return 0, err
	}
	if err := t.sendInteger("SUCC", size); err != nil {
		return 0, err
	}
	if progress != nil {
		progress.onSize(size)
	}
	return size, nil
}

func (t *trzszTransfer) recvFileData(file fileWriter, size int64, progress progressCallback) ([]byte, error) {
	defer file.Close()
	step := int64(0)
	if progress != nil {
		progress.onStep(step)
	}
	hasher := md5.New()
	for step < size {
		beginTime := time.Now()
		data, err := t.recvData()
		if err != nil {
			return nil, err
		}
		if err := writeAll(file, data); err != nil {
			return nil, err
		}
		length := int64(len(data))
		step += length
		if progress != nil {
			progress.onStep(step)
		}
		if err := t.sendInteger("SUCC", length); err != nil {
			return nil, err
		}
		if _, err := hasher.Write(data); err != nil {
			return nil, err
		}
		t.setLastChunkTime(time.Since(beginTime))
	}
	return hasher.Sum(nil), nil
}

func (t *trzszTransfer) recvFileMD5(digest []byte, progress progressCallback) error {
	expectDigest, err := t.recvBinary("MD5", false, t.getNewTimeout())
	if err != nil {
		return err
	}
	if bytes.Compare(digest, expectDigest) != 0 { // nolint:all
		return simpleTrzszError("Check MD5 failed")
	}
	if err := t.sendBinary("SUCC", digest); err != nil {
		return err
	}
	if progress != nil {
		progress.onDone()
	}
	return nil
}

func (t *trzszTransfer) recvFiles(path string, progress progressCallback) ([]string, error) {
	num, err := t.recvFileNum(progress)
	if err != nil {
		return nil, err
	}

	var localNames []string
	for i := int64(0); i < num; i++ {
		var err error
		var file fileWriter
		var localName string
		if t.transferConfig.Protocol >= kProtocolVersion3 {
			file, localName, err = t.recvFileNameV3(path, progress)
		} else {
			file, localName, err = t.recvFileName(path, progress)
		}
		if err != nil {
			return nil, err
		}

		if !containsString(localNames, localName) {
			localNames = append(localNames, localName)
		}

		if file == nil {
			continue
		}

		defer file.Close()

		size, err := t.recvFileSize(progress)
		if err != nil {
			return nil, err
		}

		var digest []byte
		if t.transferConfig.Protocol >= kProtocolVersion2 {
			digest, err = t.recvFileDataV2(file, size, progress)
		} else {
			digest, err = t.recvFileData(file, size, progress)
		}
		if err != nil {
			return nil, err
		}

		if err := t.recvFileMD5(digest, progress); err != nil {
			return nil, err
		}
	}

	return localNames, nil
}
