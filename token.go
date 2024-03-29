// Implements the lowest level of processing of PS/PDF files.
// The tokenizer is also usable with Type1 font files.
// See the higher level package pdf/parser to read PDF objects.
package tokenizer

// Code ported from the Java PDFTK library - BK 2020

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
)

type Fl = float64

// Kind is the kind of token.
type Kind uint8

const (
	_ Kind = iota
	EOF
	Float
	Integer
	String
	StringHex
	Name
	StartArray
	EndArray
	StartDic
	EndDic
	Other // include commands in content stream

	StartProc  // only valid in PostScript files
	EndProc    // idem
	CharString // PS only: binary stream, introduce by and integer and a RD or -| command
)

func (k Kind) String() string {
	switch k {
	case EOF:
		return "EOF"
	case Float:
		return "Float"
	case Integer:
		return "Integer"
	case String:
		return "String"
	case StringHex:
		return "StringHex"
	case Name:
		return "Name"
	case StartArray:
		return "StartArray"
	case EndArray:
		return "EndArray"
	case StartDic:
		return "StartDic"
	case EndDic:
		return "EndDic"
	case Other:
		return "Other"
	case StartProc:
		return "StartProc"
	case EndProc:
		return "EndProc"
	case CharString:
		return "CharString"
	default:
		return "<invalid token>"
	}
}

func isEOL(ch byte) bool {
	return ch == '\n' || ch == '\r'
}

// IsAsciiWhitespace returns true if `ch`
// is one of the ASCII whitespaces.
func IsAsciiWhitespace(ch byte) bool {
	switch ch {
	case 0, 9, 10, 12, 13, 32:
		return true
	default:
		return false
	}
}

// white space + delimiters
func isDelimiter(ch byte) bool {
	switch ch {
	case 40, 41, 60, 62, 91, 93, 123, 125, 47, 37:
		return true
	default:
		return IsAsciiWhitespace(ch)
	}
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

// Token represents a basic piece of information.
// `Value` must be interpreted according to `Kind`,
// which is left to parsing packages.
type Token struct {
	// Additional value found in the data
	// Note that it is a copy of the source bytes.
	Value []byte
	Kind  Kind
}

// Int returns the integer value of the token,
// also accepting float values and rouding them.
func (t Token) Int() (int, error) {
	// also accepts floats and round
	f, err := t.Float()
	return int(f), err
}

// Float returns the float value of the token.
func (t Token) Float() (Fl, error) {
	return strconv.ParseFloat(string(t.Value), 64)
}

// IsNumber returns `true` for integers and floats.
func (t Token) IsNumber() bool {
	return t.Kind == Integer || t.Kind == Float
}

// return true for binary stream or inline data
func (t Token) startsBinary() bool {
	s := string(t.Value)
	return t.Kind == Other && (s == "stream" || s == "ID")
}

// IsOther return true if it has `Other` kind, with the given value
func (t Token) IsOther(value string) bool {
	return t.Kind == Other && string(t.Value) == value
}

// Tokenize consume all the input, splitting it
// into tokens.
// When performance matters, you should use
// the iteration method `NextToken` of the Tokenizer type.
func Tokenize(data []byte) ([]Token, error) {
	tk := NewTokenizer(data)
	return tk.readAll()
}

func (tk *Tokenizer) readAll() ([]Token, error) {
	var out []Token
	t, err := tk.NextToken()
	for ; t.Kind != EOF && err == nil; t, err = tk.NextToken() {
		out = append(out, t)
	}
	return out, err
}

// Tokenizer is a PS/PDF tokenizer.
//
// It handles PS features like Procs and CharStrings:
// strict parsers should check for such tokens and return an error if needed.
//
// Comments are ignored.
//
// The tokenizer can't handle streams and inline image data on it's own.
//
// Regarding exponential numbers: 7.3.3 Numeric Objects:
// A conforming writer shall not use the PostScript syntax for numbers
// with non-decimal radices (such as 16#FFFE) or in exponential format
// (such as 6.02E23).
// Nonetheless, we sometimes get numbers with exponential format, so
// we support it in the tokenizer (no confusion with other types, so
// no compromise).
type Tokenizer struct {
	numberSb []byte // buffer to avoid allocations

	data []byte
	src  io.Reader // if not nil, 'data' will be read from it

	// since indirect reference require
	// to read two more tokens
	// we store the two next token

	aError error // +1
	aToken Token // +1

	aaError error // +2
	aaToken Token // +2

	pos int // main position (end of the aaToken)

	currentPos int // end of the current token
	nextPos    int // end of the +1 token

}

// NewTokenizer returns a tokenizer working on the
// given input.
func NewTokenizer(data []byte) *Tokenizer {
	tk := Tokenizer{data: data}
	tk.SetPosition(0)
	return &tk
}

// Reset allow to re-use the internal buffers allocated
// by the tokenizer.
func (tk *Tokenizer) Reset(data []byte) {
	tk.data = data
	tk.src = nil
	tk.SetPosition(0)
}

// NewTokenizerFromReader supports tokenizing an input stream,
// without knowing its length.
// The tokenizer will call Read and buffer the data.
// The error from the io.Read method is discarded:
// the internal buffer is simply not grown.
// See `SetPosition`, `SkipBytes` and `Bytes` for more information
// of the behavior in this mode.
func NewTokenizerFromReader(src io.Reader) *Tokenizer {
	tk := &Tokenizer{src: src}
	tk.SetPosition(0)
	return tk
}

// ResetFromReader allow to re-use the internal buffers allocated
// by the tokenizer.
func (tk *Tokenizer) ResetFromReader(src io.Reader) {
	tk.data = tk.data[:0]
	tk.src = src
	tk.SetPosition(0)
}

func (tk *Tokenizer) grow(size int) {
	currentLen := len(tk.data)
	if cap(tk.data) < currentLen+size {
		tk.data = append(tk.data, make([]byte, size)...)
	}
	n, _ := tk.src.Read(tk.data[currentLen : currentLen+size])
	tk.data = tk.data[:currentLen+n] // actual content read
}

// SetPosition set the position of the tokenizer in the input data.
//
// Most of the time, `NextToken` should be preferred, but this method may be used
// for example to go back to a saved position.
//
// When using an io.Reader as source, no additional buffering is performed.
func (tk *Tokenizer) SetPosition(pos int) {
	// Internally, there are two cases where NextToken() is not sufficient:
	// at the start (aToken and aaToken are empty)
	// end after skipping over bytes (aToken and aaToken are invalid)
	// in this cases, `SetPosition` force the 2 next tokenizations
	// (in the contrary, NextToken only does 1).
	tk.currentPos = pos
	tk.pos = pos
	tk.aToken, tk.aError = tk.nextToken(Token{})
	tk.nextPos = tk.pos
	tk.aaToken, tk.aaError = tk.nextToken(tk.aToken)
}

// PeekToken reads a token but does not advance the position.
// It returns a cached value, meaning it is a very cheap call.
// If the error is not nil, the return Token is garranteed to be zero.
func (pr Tokenizer) PeekToken() (Token, error) {
	return pr.aToken, pr.aError
}

// PeekPeekToken reads the token after the next but does not advance the position.
// It returns a cached value, meaning it is a very cheap call.
func (pr Tokenizer) PeekPeekToken() (Token, error) {
	return pr.aaToken, pr.aaError
}

func (pr Tokenizer) IsEOF() bool {
	tk, _ := pr.PeekToken() // delay the error checking
	return tk.Kind == EOF
}

// NextToken reads a token and advances (consuming the token).
// If EOF is reached, no error is returned, but an `EOF` token.
func (pr *Tokenizer) NextToken() (Token, error) {
	tk, err := pr.PeekToken()                     // n+1 to n
	pr.aToken, pr.aError = pr.aaToken, pr.aaError // n+2 to n+1
	pr.currentPos = pr.nextPos                    // n+1 to n
	pr.nextPos = pr.pos                           // n+2 to n

	// the tokenizer can't handle binary stream or inline data:
	// such data will be handled with a parser
	// thus, we simply stop the tokenization when we encounter them
	// to avoid useless (and maybe costly) processing
	if pr.aaToken.startsBinary() {
		pr.aaToken, pr.aaError = Token{Kind: EOF}, nil
	} else {
		pr.aaToken, pr.aaError = pr.nextToken(pr.aaToken) // read the n+3 and store it in n+2
	}
	return tk, err
}

// StreamPosition returns the position of the
// begining of a stream, taking into account
// white spaces.
// See 7.3.8.1 - General
func (pr *Tokenizer) StreamPosition() int {
	// The keyword stream that follows the stream dictionary shall be followed by an end-of-line marker
	// consisting of either a CARRIAGE RETURN and a LINE FEED or just a LINE FEED, and not by a CARRIAGE
	// RETURN alone
	pos := pr.currentPos
	if pos+2 >= len(pr.data) && pr.src != nil {
		pr.grow(2)
	}
	if pos < len(pr.data) && pr.data[pos] == '\r' {
		pos++
	}
	if pos < len(pr.data) && pr.data[pos] == '\n' {
		return pos + 1
	}
	return pos
}

// SkipBytes skips the next `n` bytes and return them. This method is useful
// to handle inline data.
// If `n` is too large, it will be truncated: no additional buffering is done.
func (pr *Tokenizer) SkipBytes(n int) []byte {
	// use currentPos, which is the position 'expected' by the caller
	target := pr.currentPos + n
	if target > len(pr.data) { // truncate if needed
		target = len(pr.data)
	}
	out := pr.data[pr.currentPos:target]
	pr.SetPosition(target)
	return out
}

// Bytes return a slice of the bytes, starting
// from the current position.
// When using an io.Reader, only the current internal buffer is returned.
func (pr Tokenizer) Bytes() []byte {
	if pr.currentPos >= len(pr.data) {
		return nil
	}
	return pr.data[pr.currentPos:]
}

// IsHexChar converts a hex character into its value and a success flag
// (see encoding/hex for details).
func IsHexChar(c byte) (uint8, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return c, false
}

const bufferSize = 1024 // should be enough for many pdf objects

// return false if EOF, true if the moved forward
func (pr *Tokenizer) read() (byte, bool) {
	if pr.pos >= len(pr.data) && pr.src != nil { // try and grow
		pr.grow(bufferSize)
	}
	if pr.pos >= len(pr.data) { // should not happen when pr.src != nil
		return 0, false
	}
	ch := pr.data[pr.pos]
	pr.pos++
	return ch, true
}

// HasEOLBeforeToken checks if EOL happens before the next token.
func (pr Tokenizer) HasEOLBeforeToken() bool {
	for i := pr.currentPos; i < len(pr.data); i++ {
		if !IsAsciiWhitespace(pr.data[i]) {
			break
		}
		if isEOL(pr.data[i]) {
			return true
		}
	}
	return false
}

// CurrentPosition return the position in the input.
// It may be used to go back if needed, using `SetPosition`.
func (pr Tokenizer) CurrentPosition() int { return pr.currentPos }

// reads and advances, mutating `pos`
func (pr *Tokenizer) nextToken(previous Token) (Token, error) {
	ch, ok := pr.read()
	for ok && IsAsciiWhitespace(ch) {
		ch, ok = pr.read()
	}
	if !ok {
		return Token{Kind: EOF}, nil
	}

	var outBuf []byte
	switch ch {
	case '[':
		return Token{Kind: StartArray}, nil
	case ']':
		return Token{Kind: EndArray}, nil
	case '{':
		return Token{Kind: StartProc}, nil
	case '}':
		return Token{Kind: EndProc}, nil
	case '/':
		for {
			ch, ok = pr.read()
			if !ok || isDelimiter(ch) {
				break
			}
			outBuf = append(outBuf, ch)
			if ch == '#' {
				h1, _ := pr.read()
				h2, _ := pr.read()
				_, err := hex.Decode([]byte{0}, []byte{h1, h2})
				if err != nil {
					return Token{}, errors.New("corrupted name object")
				}
				outBuf = append(outBuf, h1, h2)
			}
		}
		// the delimiter may be important, dont skip it
		if ok { // we moved, so its safe go back
			pr.pos--
		}
		return Token{Kind: Name, Value: outBuf}, nil
	case '>':
		ch, ok = pr.read()
		if ch != '>' {
			return Token{}, errors.New("'>' not expected")
		}
		return Token{Kind: EndDic}, nil
	case '<':
		v1, ok1 := pr.read()
		if v1 == '<' {
			return Token{Kind: StartDic}, nil
		}
		var (
			v2  byte
			ok2 bool
		)
		for {
			for ok1 && IsAsciiWhitespace(v1) {
				v1, ok1 = pr.read()
			}
			if v1 == '>' {
				break
			}
			v1, ok1 = IsHexChar(v1)
			if !ok1 {
				return Token{}, fmt.Errorf("invalid hex char %d (%s)", v1, string(rune(v1)))
			}
			v2, ok2 = pr.read()
			for ok2 && IsAsciiWhitespace(v2) {
				v2, ok2 = pr.read()
			}
			if v2 == '>' {
				ch = v1 << 4
				outBuf = append(outBuf, ch)
				break
			}
			v2, ok2 = IsHexChar(v2)
			if !ok2 {
				return Token{}, fmt.Errorf("invalid hex char %d", v2)
			}
			ch = (v1 << 4) + v2
			outBuf = append(outBuf, ch)
			v1, ok1 = pr.read()
		}
		return Token{Kind: StringHex, Value: outBuf}, nil
	case '%':
		ch, ok = pr.read()
		for ok && ch != '\r' && ch != '\n' {
			ch, ok = pr.read()
		}
		// ignore comments: go to next token
		return pr.nextToken(previous)
	case '(':
		nesting := 0
		for {
			ch, ok = pr.read()
			if !ok {
				break
			}
			if ch == '(' {
				nesting++
			} else if ch == ')' {
				nesting--
			} else if ch == '\\' {
				lineBreak := false
				ch, ok = pr.read()
				switch ch {
				case 'n':
					ch = '\n'
				case 'r':
					ch = '\r'
				case 't':
					ch = '\t'
				case 'b':
					ch = '\b'
				case 'f':
					ch = '\f'
				case '(', ')', '\\':
				case '\r':
					lineBreak = true
					ch, ok = pr.read()
					if ch != '\n' {
						pr.pos--
					}
				case '\n':
					lineBreak = true
				default:
					if ch < '0' || ch > '7' {
						break
					}
					octal := ch - '0'
					ch, ok = pr.read()
					if ch < '0' || ch > '7' {
						pr.pos--
						ch = octal
						break
					}
					octal = (octal << 3) + ch - '0'
					ch, ok = pr.read()
					if ch < '0' || ch > '7' {
						pr.pos--
						ch = octal
						break
					}
					octal = (octal << 3) + ch - '0'
					ch = octal & 0xff
					break
				}
				if lineBreak {
					continue
				}
				if !ok || ch < 0 {
					break
				}
			} else if ch == '\r' {
				ch, ok = pr.read()
				if !ok {
					break
				}
				if ch != '\n' {
					pr.pos--
					ch = '\n'
				}
			}
			if nesting == -1 {
				break
			}
			outBuf = append(outBuf, ch)
		}
		if !ok {
			return Token{}, errors.New("error reading string: unexpected EOF")
		}
		return Token{Kind: String, Value: outBuf}, nil
	default:
		pr.pos-- // we need the test char
		var token Token
		if token, ok = pr.readNumber(); ok {
			return token, nil
		}
		ch, ok = pr.read() // we went back before parsing a number
		outBuf = append(outBuf, ch)
		ch, ok = pr.read()
		for !isDelimiter(ch) {
			outBuf = append(outBuf, ch)
			ch, ok = pr.read()
		}
		if ok {
			pr.pos--
		}

		if cmd := string(outBuf); cmd == "RD" || cmd == "-|" {
			// return the next CharString instead
			if previous.Kind == Integer {
				f, err := previous.Int()
				if err != nil {
					return Token{}, fmt.Errorf("invalid charstring length: %s", err)
				}
				return pr.readCharString(f), nil
			} else {
				return Token{}, errors.New("expected INTEGER before -| or RD")
			}
		}
		return Token{Kind: Other, Value: outBuf}, nil
	}
}

// accept PS syntax (radix and exponents)
// return false if it is not a number
func (pr *Tokenizer) readNumber() (Token, bool) {
	markedPos := pr.pos

	pr.numberSb = pr.numberSb[:0]
	var radix string

	c, ok := pr.read() // one char is OK
	hasDigit := false
	// optional + or -
	if c == '+' || c == '-' {
		pr.numberSb = append(pr.numberSb, c)
		c, _ = pr.read()
	}

	// optional digits
	for isDigit(c) {
		pr.numberSb = append(pr.numberSb, c)
		c, ok = pr.read()
		hasDigit = true
	}

	numberRequired := true
	// optional .
	if c == '.' {
		pr.numberSb = append(pr.numberSb, c)
		c, ok = pr.read()
		// a float may terminate after . (like in 4.)
		numberRequired = false
	} else if c == '#' {
		// PostScript radix number takes the form base#number
		radix = string(pr.numberSb)
		pr.numberSb = pr.numberSb[:0]
		c, ok = pr.read()
	} else if len(pr.numberSb) == 0 || !hasDigit {
		// failure
		pr.pos = markedPos
		return Token{}, false
	} else if c == 'E' || c == 'e' {
		// optional minus
		pr.numberSb = append(pr.numberSb, c)
		c, ok = pr.read()
		if c == '-' {
			pr.numberSb = append(pr.numberSb, c)
			c, ok = pr.read()
		}
	} else {
		// integer
		if ok {
			pr.pos--
		}
		return Token{Value: copyBytes(pr.numberSb), Kind: Integer}, true
	}

	// check required digit
	if numberRequired && !isDigit(c) {
		// failure
		pr.pos = markedPos
		return Token{}, false
	}

	// optional digits
	for isDigit(c) {
		pr.numberSb = append(pr.numberSb, c)
		c, ok = pr.read()
	}

	if ok {
		pr.pos--
	}
	if radix != "" {
		intRadix, _ := strconv.Atoi(radix)
		valInt, _ := strconv.ParseInt(string(pr.numberSb), intRadix, 0)
		return Token{Value: []byte(strconv.Itoa(int(valInt))), Kind: Integer}, true
	}
	return Token{Value: copyBytes(pr.numberSb), Kind: Float}, true
}

// reads a binary CharString.
func (pr *Tokenizer) readCharString(length int) Token {
	pr.pos++ // space
	maxL := pr.pos + length
	if maxL >= len(pr.data) && pr.src != nil { // try to grow
		pr.grow(maxL - len(pr.data))
	}
	if maxL >= len(pr.data) {
		maxL = len(pr.data)
	}
	out := Token{Value: copyBytes(pr.data[pr.pos:maxL]), Kind: CharString}
	pr.pos += length
	return out
}

func copyBytes(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	return out
}
