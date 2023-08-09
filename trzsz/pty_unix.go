//go:build !windows

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
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type trzszPty struct {
	Stdin  io.ReadWriteCloser
	Stdout io.ReadWriteCloser
	ptmx   *os.File
	cmd    *exec.Cmd
	ch     chan os.Signal
	closed atomic.Bool
}

// 生成一个新的伪终端, 执行相应的命令, 返回终端的文件描述符
func spawn(name string, arg ...string) (*trzszPty, error) {
	// spawn a pty
	cmd := exec.Command(name, arg...)
	ptmx, err := pty.Start(cmd) // 在新的伪终端非阻塞执行 cmd ，并返回 pty(*os.File)
	if err != nil {
		return nil, err
	}
	return &trzszPty{Stdin: ptmx, Stdout: ptmx, ptmx: ptmx, cmd: cmd}, nil
}

// 注册监听系统信号，调整pty窗口大小
func (t *trzszPty) OnResize(setTerminalColumns func(int32)) {
	if t.ch != nil {
		return
	}
	t.ch = make(chan os.Signal, 1)
	signal.Notify(t.ch, syscall.SIGWINCH) // 注册系统信号
	go func() {
		defer func() { signal.Stop(t.ch); close(t.ch) }()
		for range t.ch {
			if t.closed.Load() {
				break // 退出 goroutine
			}
			size, err := pty.GetsizeFull(os.Stdin) // 改变窗口大小
			if err != nil {
				continue
			}
			_ = pty.Setsize(t.ptmx, size)
			if setTerminalColumns != nil {
				setTerminalColumns(int32(size.Cols))
			}
		}
	}()
	t.ch <- syscall.SIGWINCH
}

// 获取 pty 的列数
func (t *trzszPty) GetColumns() (int32, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return 0, nil
	}
	size, err := pty.GetsizeFull(os.Stdin)
	if err != nil {
		return 0, err
	}
	return int32(size.Cols), nil
}

// 关闭 pty 文件描述符
func (t *trzszPty) Close() {
	if t.closed.Load() {
		return
	}
	t.closed.Store(true)
	if t.ch != nil {
		t.ch <- syscall.SIGWINCH // 发出信号关闭调整窗口大小 goroutine
	}
	t.ptmx.Close()
}

// 等待 pty 关闭
func (t *trzszPty) Wait() {
	_ = t.cmd.Wait()
}

// 终止 pty 进程
func (t *trzszPty) Terminate() {
	_ = t.cmd.Process.Signal(syscall.SIGTERM)
}

// 获取退出代码
func (t *trzszPty) ExitCode() int {
	return t.cmd.ProcessState.ExitCode()
}

// 检查文件路径是否可写
func syscallAccessWok(path string) error {
	return syscall.Access(path, unix.W_OK)
}

// 检查文件路径是否可读
func syscallAccessRok(path string) error {
	return syscall.Access(path, unix.R_OK)
}

func enableVirtualTerminal() (uint32, uint32, error) {
	return 0, 0, nil
}

func resetVirtualTerminal(inMode, outMode uint32) error {
	return nil
}

func setupConsoleOutput() {
}
