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
	"encoding/json"
	"fmt"
	"strconv"

	"golang.org/x/text/encoding/charmap"
)

type unicode string

type escapeArray [][]byte

const escapeLeaderByte = '\xee' // 约定的转义头

// 解析到 JSON
func (s unicode) MarshalJSON() ([]byte, error) {
	b := new(bytes.Buffer)
	b.WriteByte('"')
	for _, c := range s {
		if c < 128 && strconv.IsPrint(c) {
			b.WriteRune(c)
		} else {
			b.WriteString(fmt.Sprintf("\\u%04x", c)) // \u 后面接4个16进制数, 表示两个字节的转义
		}
	}
	b.WriteByte('"')
	return b.Bytes(), nil
}

// 获取转义字符 (第一个转义字符是原字符, 第二个转义字符是头字符, 第三个字符是转义后的字符; 类似映射转换)
func getEscapeChars(escapeAll bool) [][]unicode {
	escapeChars := [][]unicode{
		{"\u00ee", "\u00ee\u00ee"},
		{"\u007e", "\u00ee\u0031"},
	}
	if escapeAll {
		const chars = unicode("\x02\x10\x1b\x1d\u009d")
		for i, c := range chars {
			escapeChars = append(escapeChars, []unicode{unicode(c), "\u00ee" + unicode(byte(i+0x41))}) // "i+65" 即 A B C D
		}
	}
	return escapeChars
}

// 从 转义字符 转换为 转义表
func escapeCharsToCodes(escapeChars []interface{}) ([][]byte, error) {
	escapeCodes := make([][]byte, len(escapeChars))
	encoder := charmap.ISO8859_1.NewEncoder()
	for i, v := range escapeChars {
		a, ok := v.([]interface{})
		if !ok {
			return nil, simpleTrzszError("Escape chars invalid: %v", v)
		}
		if len(a) != 2 {
			return nil, simpleTrzszError("Escape chars invalid: %v", v)
		}
		b, ok := a[0].(string)
		if !ok {
			return nil, simpleTrzszError("Escape chars invalid: %v", v)
		}
		bb, err := encoder.Bytes([]byte(b))
		if err != nil {
			return nil, err
		}
		if len(bb) != 1 {
			return nil, simpleTrzszError("Escape chars invalid: %v", v)
		}
		c, ok := a[1].(string)
		if !ok {
			return nil, simpleTrzszError("Escape chars invalid: %v", v)
		}
		cc, err := encoder.Bytes([]byte(c))
		if err != nil {
			return nil, err
		}
		if len(cc) != 2 {
			return nil, simpleTrzszError("Escape chars invalid: %v", v)
		}
		if cc[0] != escapeLeaderByte {
			return nil, simpleTrzszError("Escape chars invalid: %v", v)
		}
		escapeCodes[i] = make([]byte, 3)
		escapeCodes[i][0] = bb[0]
		escapeCodes[i][1] = cc[0]
		escapeCodes[i][2] = cc[1]
	}
	return escapeCodes, nil
}

// 从 json 解析 转义表
func (c *escapeArray) UnmarshalJSON(data []byte) error {
	var codes []interface{}
	if err := json.Unmarshal(data, &codes); err != nil {
		return err
	}
	var err error
	*c, err = escapeCharsToCodes(codes)
	return err
}

// 根据转义表 转义字节序列(相当于加密过程)
func escapeData(data []byte, escapeCodes [][]byte) []byte {
	if len(escapeCodes) == 0 {
		return data
	}

	buf := make([]byte, len(data)*2)
	idx := 0
	for _, d := range data {
		escapeIdx := -1
		for j, e := range escapeCodes {
			if d == e[0] {
				escapeIdx = j
				break
			}
		}
		if escapeIdx < 0 {
			buf[idx] = d
			idx++
		} else {
			buf[idx] = escapeCodes[escapeIdx][1]
			idx++
			buf[idx] = escapeCodes[escapeIdx][2]
			idx++
		}
	}
	return buf[:idx]
}

// 解析 已转义的序列;
// 如果dst是空,则自动分配一个空间, 如果dst不空, 转义结果长度为 len(dst);
// 返回值是 [转义的结果, 剩余未解析的data序列, 错误] (相当于解密过程)
func unescapeData(data []byte, escapeCodes [][]byte, dst []byte) ([]byte, []byte, error) {
	if len(escapeCodes) == 0 {
		return data, nil, nil
	}

	size := len(data)
	buf := dst
	if len(buf) == 0 {
		buf = make([]byte, size)
	}
	idx := 0
	for i := 0; i < size; i++ {
		if data[i] == escapeLeaderByte {
			if i == size-1 {
				return buf[:idx], data[i:], nil
			}
			i++ // 跳过 头字节
			b := data[i]
			escaped := false // 是否已转义
			for _, e := range escapeCodes {
				if b == e[2] {
					buf[idx] = e[0]
					escaped = true
					break
				}
			}
			if !escaped {
				return nil, nil, simpleTrzszError("Unknown escape code: %v", b)
			}
		} else {
			buf[idx] = data[i]
		}
		idx++
		if idx == len(buf) {
			return buf[:idx], data[i+1:], nil
		}
	}
	return buf[:idx], nil, nil
}
