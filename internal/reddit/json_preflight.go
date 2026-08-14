package reddit

import (
	"bytes"
	"context"
	"unicode/utf16"
	"unicode/utf8"
)

type preflightLimits struct {
	maxThings         int
	maxComments       int
	maxMoreIDs        int
	maxBodyBytes      int
	maxTotalBodyBytes int64
}

type preflightKey byte

const (
	preflightKeyOther preflightKey = iota
	preflightKeyKind
	preflightKeyData
	preflightKeyBody
	preflightKeyChildren
	preflightKeyThings
)

type preflightPhase byte

const (
	phaseObjectKeyOrEnd preflightPhase = iota
	phaseObjectKey
	phaseObjectColon
	phaseObjectValue
	phaseObjectCommaOrEnd
	phaseArrayValueOrEnd
	phaseArrayValue
	phaseArrayCommaOrEnd
)

type preflightFrame struct {
	kind         byte
	phase        preflightPhase
	pendingKey   preflightKey
	arrayKey     preflightKey
	arrayType    byte
	thing        bool
	thingT1      bool
	objectKey    preflightKey
	parentThing  bool
	bodyBytes    int
	bodyPresent  bool
	bodyInvalid  bool
	objectFields int
}

type preflightString struct {
	start      int
	end        int
	decodedLen int
}

type preflightScanner struct {
	ctx     context.Context
	payload []byte
	limits  preflightLimits
	offset  int
	stack   []preflightFrame
	root    bool

	things            int
	comments          int
	moreIDs           int
	bodyBytes         int64
	auxiliaryElements int
	objectFields      int
}

// scanJSONPreflight validates JSON syntax and the allocation-driving cardinalities
// in one zero-copy lexical pass. Unlike json.Decoder.Token, ordinary field names,
// scalar values, and comment bodies are not materialized as interface values or
// duplicate strings before the bounded generic decode.
func scanJSONPreflight(ctx context.Context, payload []byte, limits preflightLimits) error {
	if ctx == nil || len(payload) == 0 || !utf8.Valid(payload) {
		return errMalformedResponse
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scanner := preflightScanner{
		ctx:     ctx,
		payload: payload,
		limits:  limits,
		stack:   make([]preflightFrame, 0, 64),
	}
	return scanner.scan()
}

func (scanner *preflightScanner) scan() error {
	for steps := 0; ; steps++ {
		if steps%256 == 0 {
			if err := scanner.ctx.Err(); err != nil {
				return err
			}
		}
		if err := scanner.skipWhitespace(); err != nil {
			return err
		}
		if len(scanner.stack) == 0 {
			if scanner.root {
				if scanner.offset != len(scanner.payload) {
					return errMalformedResponse
				}
				return scanner.ctx.Err()
			}
			if err := scanner.scanValue(-1); err != nil {
				return err
			}
			continue
		}

		index := len(scanner.stack) - 1
		frame := &scanner.stack[index]
		switch frame.phase {
		case phaseObjectKeyOrEnd:
			if scanner.take('}') {
				if err := scanner.closeFrame(); err != nil {
					return err
				}
				continue
			}
			if err := scanner.scanObjectKey(frame); err != nil {
				return err
			}
		case phaseObjectKey:
			if err := scanner.scanObjectKey(frame); err != nil {
				return err
			}
		case phaseObjectColon:
			if !scanner.take(':') {
				return errMalformedResponse
			}
			frame.phase = phaseObjectValue
		case phaseObjectValue:
			if err := scanner.scanValue(index); err != nil {
				return err
			}
		case phaseObjectCommaOrEnd:
			if scanner.take(',') {
				frame.phase = phaseObjectKey
				continue
			}
			if !scanner.take('}') {
				return errMalformedResponse
			}
			if err := scanner.closeFrame(); err != nil {
				return err
			}
		case phaseArrayValueOrEnd:
			if scanner.take(']') {
				if err := scanner.closeFrame(); err != nil {
					return err
				}
				continue
			}
			if err := scanner.scanValue(index); err != nil {
				return err
			}
		case phaseArrayValue:
			if err := scanner.scanValue(index); err != nil {
				return err
			}
		case phaseArrayCommaOrEnd:
			if scanner.take(',') {
				frame.phase = phaseArrayValue
				continue
			}
			if !scanner.take(']') {
				return errMalformedResponse
			}
			if err := scanner.closeFrame(); err != nil {
				return err
			}
		default:
			return errMalformedResponse
		}
	}
}

func (scanner *preflightScanner) scanObjectKey(frame *preflightFrame) error {
	value, err := scanner.scanString()
	if err != nil {
		return err
	}
	if frame.objectFields == maxObjectFields || scanner.objectFields == maxObjectFieldsPerResponse {
		return errThingLimit
	}
	frame.objectFields++
	scanner.objectFields++
	frame.pendingKey = scanner.classifyKey(value)
	frame.phase = phaseObjectColon
	return nil
}

func (scanner *preflightScanner) scanValue(parentIndex int) error {
	if scanner.offset >= len(scanner.payload) {
		return errMalformedResponse
	}
	valueType := byte('p')
	var text preflightString
	composite := byte(0)
	switch scanner.payload[scanner.offset] {
	case '{', '[':
		composite = scanner.payload[scanner.offset]
		valueType = composite
		scanner.offset++
	case '"':
		var err error
		text, err = scanner.scanString()
		if err != nil {
			return err
		}
		valueType = 's'
	case 'n':
		if !scanner.takeLiteral("null") {
			return errMalformedResponse
		}
		valueType = '0'
	case 't':
		if !scanner.takeLiteral("true") {
			return errMalformedResponse
		}
	case 'f':
		if !scanner.takeLiteral("false") {
			return errMalformedResponse
		}
	default:
		if err := scanner.scanNumber(); err != nil {
			return err
		}
	}

	parentKey := preflightKeyOther
	parentKind := byte(0)
	parentThing := false
	parentArrayKey := preflightKeyOther
	if parentIndex < 0 {
		if scanner.root {
			return errMalformedResponse
		}
		scanner.root = true
	} else {
		parent := &scanner.stack[parentIndex]
		parentKey = parent.pendingKey
		parentKind = parent.kind
		parentThing = parent.thing
		parentArrayKey = parent.arrayKey
		if err := scanner.consumeValue(parentIndex, valueType, text); err != nil {
			return err
		}
		if parent.kind == '{' {
			parent.pendingKey = preflightKeyOther
			parent.phase = phaseObjectCommaOrEnd
		} else {
			parent.phase = phaseArrayCommaOrEnd
		}
	}

	if composite == 0 {
		return nil
	}
	if len(scanner.stack) == maxJSONNestingDepth {
		return errThingLimit
	}
	frame := preflightFrame{kind: composite}
	if composite == '{' {
		frame.phase = phaseObjectKeyOrEnd
	} else {
		frame.phase = phaseArrayValueOrEnd
	}
	if parentIndex >= 0 {
		if parentKind == '{' {
			frame.objectKey = parentKey
			frame.parentThing = composite == '{' && frame.objectKey == preflightKeyData && parentThing
			if composite == '[' {
				frame.arrayKey = parentKey
			}
		}
		if composite == '{' && parentKind == '[' &&
			(parentArrayKey == preflightKeyChildren || parentArrayKey == preflightKeyThings) {
			frame.thing = true
		}
	}
	scanner.stack = append(scanner.stack, frame)
	return nil
}

func (scanner *preflightScanner) consumeValue(parentIndex int, valueType byte, text preflightString) error {
	frame := &scanner.stack[parentIndex]
	if frame.kind == '{' {
		if frame.phase != phaseObjectValue {
			return errMalformedResponse
		}
		if frame.thing && frame.pendingKey == preflightKeyKind {
			frame.thingT1 = valueType == 's' && scanner.stringEqualsT1(text)
		}
		if frame.thing && frame.pendingKey == preflightKeyData {
			frame.bodyBytes = 0
			frame.bodyPresent = false
			frame.bodyInvalid = false
		}
		if frame.pendingKey == preflightKeyBody && frame.objectKey == preflightKeyData && frame.parentThing {
			frame.bodyInvalid = valueType != 's' && valueType != '0'
			frame.bodyPresent = valueType == 's'
			frame.bodyBytes = 0
			if valueType == 's' {
				frame.bodyBytes = text.decodedLen
			}
		}
		return nil
	}
	if frame.kind != '[' || (frame.phase != phaseArrayValueOrEnd && frame.phase != phaseArrayValue) {
		return errMalformedResponse
	}
	switch frame.arrayKey {
	case preflightKeyChildren:
		arrayType := byte(0)
		switch valueType {
		case '{':
			arrayType = 't'
		case 's':
			arrayType = 'm'
		default:
			return errMalformedResponse
		}
		if frame.arrayType != 0 && frame.arrayType != arrayType {
			return errMalformedResponse
		}
		frame.arrayType = arrayType
		if arrayType == 't' {
			if scanner.things == scanner.limits.maxThings {
				return errThingLimit
			}
			scanner.things++
		} else {
			if scanner.moreIDs == scanner.limits.maxMoreIDs {
				return errMoreIDLimit
			}
			scanner.moreIDs++
		}
	case preflightKeyThings:
		if valueType != '{' {
			return errMalformedResponse
		}
		if scanner.things == scanner.limits.maxThings {
			return errThingLimit
		}
		scanner.things++
	default:
		if scanner.auxiliaryElements == maxAuxiliaryArrayElements {
			return errThingLimit
		}
		scanner.auxiliaryElements++
	}
	return nil
}

func (scanner *preflightScanner) closeFrame() error {
	if len(scanner.stack) == 0 {
		return errMalformedResponse
	}
	frame := scanner.stack[len(scanner.stack)-1]
	if frame.thing {
		if frame.thingT1 {
			if scanner.comments == scanner.limits.maxComments {
				return errCommentLimit
			}
			scanner.comments++
		}
		if err := scanner.accountThingBody(frame); err != nil {
			return err
		}
	}
	scanner.stack = scanner.stack[:len(scanner.stack)-1]
	if frame.parentThing && len(scanner.stack) > 0 {
		parent := &scanner.stack[len(scanner.stack)-1]
		parent.bodyBytes = frame.bodyBytes
		parent.bodyPresent = frame.bodyPresent
		parent.bodyInvalid = frame.bodyInvalid
	}
	return nil
}

func (scanner *preflightScanner) accountThingBody(frame preflightFrame) error {
	if !frame.thingT1 {
		return nil
	}
	if frame.bodyInvalid {
		return errMalformedResponse
	}
	if !frame.bodyPresent {
		return nil
	}
	if frame.bodyBytes > scanner.limits.maxBodyBytes {
		return errCommentBodyTooLarge
	}
	if int64(frame.bodyBytes) > scanner.limits.maxTotalBodyBytes-scanner.bodyBytes {
		return errCommentBodiesTooLarge
	}
	scanner.bodyBytes += int64(frame.bodyBytes)
	return nil
}

func (scanner *preflightScanner) scanString() (preflightString, error) {
	if scanner.offset >= len(scanner.payload) || scanner.payload[scanner.offset] != '"' {
		return preflightString{}, errMalformedResponse
	}
	result := preflightString{start: scanner.offset}
	scanner.offset++
	for scanner.offset < len(scanner.payload) {
		if scanner.offset%4096 == 0 {
			if err := scanner.ctx.Err(); err != nil {
				return preflightString{}, err
			}
		}
		value := scanner.payload[scanner.offset]
		switch {
		case value == '"':
			scanner.offset++
			result.end = scanner.offset
			return result, nil
		case value < 0x20:
			return preflightString{}, errMalformedResponse
		case value != '\\':
			result.decodedLen++
			scanner.offset++
		default:
			scanner.offset++
			if scanner.offset >= len(scanner.payload) {
				return preflightString{}, errMalformedResponse
			}
			escape := scanner.payload[scanner.offset]
			scanner.offset++
			switch escape {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				result.decodedLen++
			case 'u':
				first, ok := scanner.takeHexRune()
				if !ok {
					return preflightString{}, errMalformedResponse
				}
				decoded := rune(first)
				if utf16.IsSurrogate(decoded) {
					if 0xD800 <= first && first <= 0xDBFF && scanner.offset+6 <= len(scanner.payload) &&
						scanner.payload[scanner.offset] == '\\' && scanner.payload[scanner.offset+1] == 'u' {
						saved := scanner.offset
						scanner.offset += 2
						second, secondOK := scanner.takeHexRune()
						if secondOK && 0xDC00 <= second && second <= 0xDFFF {
							decoded = utf16.DecodeRune(rune(first), rune(second))
						} else {
							scanner.offset = saved
							decoded = utf8.RuneError
						}
					} else {
						decoded = utf8.RuneError
					}
				}
				result.decodedLen += utf8.RuneLen(decoded)
			default:
				return preflightString{}, errMalformedResponse
			}
		}
	}
	return preflightString{}, errMalformedResponse
}

func (scanner *preflightScanner) takeHexRune() (uint16, bool) {
	if scanner.offset+4 > len(scanner.payload) {
		return 0, false
	}
	var value uint16
	for range 4 {
		digit := scanner.payload[scanner.offset]
		scanner.offset++
		value <<= 4
		switch {
		case '0' <= digit && digit <= '9':
			value |= uint16(digit - '0')
		case 'a' <= digit && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case 'A' <= digit && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func (scanner *preflightScanner) scanNumber() error {
	start := scanner.offset
	if scanner.take('-') && scanner.offset == len(scanner.payload) {
		return errMalformedResponse
	}
	if scanner.take('0') {
		if scanner.offset < len(scanner.payload) && scanner.payload[scanner.offset] >= '0' && scanner.payload[scanner.offset] <= '9' {
			return errMalformedResponse
		}
	} else {
		if scanner.offset >= len(scanner.payload) || scanner.payload[scanner.offset] < '1' || scanner.payload[scanner.offset] > '9' {
			return errMalformedResponse
		}
		if _, err := scanner.scanDecimalDigits(); err != nil {
			return err
		}
	}
	if scanner.take('.') {
		fractionDigits, err := scanner.scanDecimalDigits()
		if err != nil {
			return err
		}
		if fractionDigits == 0 {
			return errMalformedResponse
		}
	}
	if scanner.offset < len(scanner.payload) && (scanner.payload[scanner.offset] == 'e' || scanner.payload[scanner.offset] == 'E') {
		scanner.offset++
		if scanner.offset < len(scanner.payload) && (scanner.payload[scanner.offset] == '+' || scanner.payload[scanner.offset] == '-') {
			scanner.offset++
		}
		exponentDigits, err := scanner.scanDecimalDigits()
		if err != nil {
			return err
		}
		if exponentDigits == 0 {
			return errMalformedResponse
		}
	}
	if scanner.offset == start {
		return errMalformedResponse
	}
	return nil
}

func (scanner *preflightScanner) scanDecimalDigits() (int, error) {
	start := scanner.offset
	for scanner.offset < len(scanner.payload) && scanner.payload[scanner.offset] >= '0' && scanner.payload[scanner.offset] <= '9' {
		if scanner.offset%4096 == 0 {
			if err := scanner.ctx.Err(); err != nil {
				return 0, err
			}
		}
		scanner.offset++
	}
	return scanner.offset - start, nil
}

func (scanner *preflightScanner) classifyKey(value preflightString) preflightKey {
	switch value.decodedLen {
	case 4:
		switch {
		case scanner.stringEqualsASCII(value, "kind"):
			return preflightKeyKind
		case scanner.stringEqualsASCII(value, "data"):
			return preflightKeyData
		case scanner.stringEqualsASCII(value, "body"):
			return preflightKeyBody
		}
	case 6:
		if scanner.stringEqualsASCII(value, "things") {
			return preflightKeyThings
		}
	case 8:
		if scanner.stringEqualsASCII(value, "children") {
			return preflightKeyChildren
		}
	}
	return preflightKeyOther
}

func (scanner *preflightScanner) stringEqualsT1(value preflightString) bool {
	return value.decodedLen == 2 && scanner.stringEqualsASCII(value, "t1")
}

// stringEqualsASCII compares a validated JSON string with a small protocol token
// without allocating a decoded Go string. All recognized keys and kinds are ASCII;
// any non-ASCII escape or raw byte therefore proves inequality immediately.
func (scanner *preflightScanner) stringEqualsASCII(value preflightString, want string) bool {
	if value.decodedLen != len(want) || value.start < 0 || value.end > len(scanner.payload) || value.end-value.start < 2 {
		return false
	}
	offset := value.start + 1
	end := value.end - 1
	wantOffset := 0
	for offset < end {
		decoded := scanner.payload[offset]
		offset++
		if decoded == '\\' {
			if offset >= end {
				return false
			}
			escape := scanner.payload[offset]
			offset++
			switch escape {
			case '"', '\\', '/':
				decoded = escape
			case 'b':
				decoded = '\b'
			case 'f':
				decoded = '\f'
			case 'n':
				decoded = '\n'
			case 'r':
				decoded = '\r'
			case 't':
				decoded = '\t'
			case 'u':
				codePoint, ok := decodeHexRuneAt(scanner.payload, offset, end)
				if !ok || codePoint > utf8.RuneSelf-1 {
					return false
				}
				offset += 4
				decoded = byte(codePoint)
			default:
				return false
			}
		}
		if wantOffset >= len(want) || decoded != want[wantOffset] {
			return false
		}
		wantOffset++
	}
	return wantOffset == len(want)
}

func decodeHexRuneAt(payload []byte, offset, end int) (uint16, bool) {
	if offset < 0 || offset+4 > end {
		return 0, false
	}
	var value uint16
	for index := range 4 {
		digit := payload[offset+index]
		value <<= 4
		switch {
		case '0' <= digit && digit <= '9':
			value |= uint16(digit - '0')
		case 'a' <= digit && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case 'A' <= digit && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func (scanner *preflightScanner) skipWhitespace() error {
	for scanner.offset < len(scanner.payload) {
		if scanner.offset%4096 == 0 {
			if err := scanner.ctx.Err(); err != nil {
				return err
			}
		}
		switch scanner.payload[scanner.offset] {
		case ' ', '\t', '\n', '\r':
			scanner.offset++
		default:
			return nil
		}
	}
	return nil
}

func (scanner *preflightScanner) take(value byte) bool {
	if scanner.offset >= len(scanner.payload) || scanner.payload[scanner.offset] != value {
		return false
	}
	scanner.offset++
	return true
}

func (scanner *preflightScanner) takeLiteral(value string) bool {
	if len(scanner.payload)-scanner.offset < len(value) ||
		!bytes.Equal(scanner.payload[scanner.offset:scanner.offset+len(value)], []byte(value)) {
		return false
	}
	scanner.offset += len(value)
	return true
}
