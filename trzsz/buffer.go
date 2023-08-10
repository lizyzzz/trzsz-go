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
	"time"
)

// 发送缓存类型
type trzszBuffer struct {
	bufCh      chan []byte
	stopCh     chan bool
	nextBuf    []byte
	nextIdx    int // 下一个可读取的下表
	readBuf    bytes.Buffer
	timeout    <-chan time.Time // 上一个超时时间
	newTimeout <-chan time.Time // 新的超时时间
}

func newTrzszBuffer() *trzszBuffer {
	return &trzszBuffer{bufCh: make(chan []byte, 10000), stopCh: make(chan bool, 1)}
}

// 往 trzszbuffer 发送数据 buf
func (b *trzszBuffer) addBuffer(buf []byte) {
	b.bufCh <- buf
}

// 停止接收数据, 往 stopCh 发送 true
func (b *trzszBuffer) stopBuffer() {
	select {
	case b.stopCh <- true:
	default:
	}
}

// 把 bufCh 的 数据清空
func (b *trzszBuffer) drainBuffer() {
	for {
		select {
		case <-b.bufCh:
		default:
			return
		}
	}
}

// 从 trzszbuffer 取出数据 (返回 nextBuf 或从 bufCh 读取), 并置空
func (b *trzszBuffer) popBuffer() []byte {
	if b.nextBuf != nil && b.nextIdx < len(b.nextBuf) {
		buf := b.nextBuf[b.nextIdx:]
		b.nextBuf = nil
		b.nextIdx = 0
		return buf
	}
	select {
	case buf := <-b.bufCh:
		return buf
	default:
		b.nextBuf = nil
		b.nextIdx = 0
		return nil
	}
}

// 设置新的 timeout 接收channel
func (b *trzszBuffer) setNewTimeout(timeout <-chan time.Time) {
	b.newTimeout = timeout
}

// 返回 nextBuffer 但不置空
func (b *trzszBuffer) nextBuffer() ([]byte, error) {
	if b.nextBuf != nil && b.nextIdx < len(b.nextBuf) {
		return b.nextBuf[b.nextIdx:], nil
	}
	for {
		select {
		case b.nextBuf = <-b.bufCh:
			b.nextIdx = 0
			return b.nextBuf, nil
		case <-b.stopCh:
			return nil, errStopped
		case <-b.timeout:
			if b.newTimeout != nil {
				b.timeout = b.newTimeout
				b.newTimeout = nil
				continue
			}
			return nil, errReceiveDataTimeout
		}
	}
}

// 以 \n 为分隔符读取一行
func (b *trzszBuffer) readLine(mayHasJunk bool, timeout <-chan time.Time) ([]byte, error) {
	b.readBuf.Reset()
	b.timeout = timeout
	b.newTimeout = nil
	for {
		buf, err := b.nextBuffer()
		if err != nil {
			return nil, err
		}
		newLineIdx := bytes.IndexByte(buf, '\n')
		if newLineIdx >= 0 {
			b.nextIdx += newLineIdx + 1 // +1 to ignroe the '\n'
			buf = buf[0:newLineIdx]
		} else {
			b.nextIdx += len(buf)
		}
		if bytes.IndexByte(buf, '\x03') >= 0 { // `ctrl + c` to interrupt
			return nil, simpleTrzszError("Interrupted") // 存在 ctrl + c
		}
		b.readBuf.Write(buf)
		if newLineIdx >= 0 {
			if mayHasJunk && b.readBuf.Len() > 0 && b.readBuf.Bytes()[b.readBuf.Len()-1] == '\r' {
				b.readBuf.Truncate(b.readBuf.Len() - 1) // 保留前 n - 1 位
				continue                                // 可以多次写到 readBuf 中
			}
			return b.readBuf.Bytes(), nil
		}
	}
}

// 读取固定长度的 字节
func (b *trzszBuffer) readBinary(size int, timeout <-chan time.Time) ([]byte, error) {
	b.readBuf.Reset()
	if b.readBuf.Cap() < size {
		b.readBuf.Grow(size)
	}
	b.timeout = timeout
	b.newTimeout = nil
	for b.readBuf.Len() < size {
		buf, err := b.nextBuffer()
		if err != nil {
			return nil, err
		}
		left := size - b.readBuf.Len()
		if len(buf) > left {
			b.nextIdx += left
			buf = buf[0:left]
		} else {
			b.nextIdx += len(buf)
		}
		b.readBuf.Write(buf)
	}
	return b.readBuf.Bytes(), nil
}

// 是否是特定的字符(a-z, A-Z, 0-9, #:+/=)
func isTrzszLetter(b byte) bool {
	if 'a' <= b && b <= 'z' {
		return true
	}
	if 'A' <= b && b <= 'Z' {
		return true
	}
	if '0' <= b && b <= '9' {
		return true
	}
	if b == '#' || b == ':' || b == '+' || b == '/' || b == '=' {
		return true
	}
	return false
}

// 读取窗口中的一行(以 ! 为分割)
func (b *trzszBuffer) readLineOnWindows(timeout <-chan time.Time) ([]byte, error) {
	b.readBuf.Reset()
	b.timeout = timeout
	b.newTimeout = nil
	lastByte := byte('\x1b')
	skipVT100 := false
	hasNewline := false
	mayDuplicate := false
	hasCursorHome := false
	preHasCursorHome := false
	for {
		buf, err := b.nextBuffer()
		if err != nil {
			return nil, err
		}
		newLineIdx := bytes.IndexByte(buf, '!') // 以 '!' 分割一行?
		if newLineIdx >= 0 {
			b.nextIdx += newLineIdx + 1                         // +1 to ignroe the newline
			if b.nextIdx < len(buf) && buf[b.nextIdx] == '\n' { // 也跳过 \n
				b.nextIdx++
			}
			buf = buf[0:newLineIdx]
		} else {
			b.nextIdx += len(buf)
		}
		// 检查序列中是否有其他字符
		for i := 0; i < len(buf); i++ {
			c := buf[i]
			if c == '\x03' { // `ctrl + c` to interrupt
				return nil, simpleTrzszError("Interrupted")
			}
			if c == '\n' {
				hasNewline = true
			}
			if skipVT100 { // 转义字符序列的开始
				// \x1b[H (\x1b[m;nH 表示 光标移到 m行 n列, 默认是 1,1 )
				if isVT100End(c) {
					skipVT100 = false
					// moving the cursor may result in duplicate characters
					if c == 'H' && lastByte >= '0' && lastByte <= '9' {
						mayDuplicate = true // 移动光标可能会重复字符
					}
				}
				if lastByte == '[' && c == 'H' {
					hasCursorHome = true // 有位于 1, 1 的光标
				}
				lastByte = c
			} else if c == '\x1b' {
				skipVT100 = true
				lastByte = c
			} else if isTrzszLetter(c) { // 只保留特定字符
				if mayDuplicate {
					mayDuplicate = false
					// skip the duplicate characters, e.g., the "8" in "8\r\n\x1b[25;119H8".
					bytes := b.readBuf.Bytes()
					if hasNewline && len(bytes) > 0 && (c == bytes[len(bytes)-1] || preHasCursorHome) {
						bytes[len(bytes)-1] = c
						continue
					}
				}
				b.readBuf.WriteByte(c)
				preHasCursorHome = hasCursorHome
				hasCursorHome = false
				hasNewline = false
			}
		}
		if newLineIdx >= 0 && b.readBuf.Len() > 0 && !skipVT100 {
			return b.readBuf.Bytes(), nil
		}
	}
}
