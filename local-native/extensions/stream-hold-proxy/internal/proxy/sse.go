package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type terminalKind int

const (
	terminalNone terminalKind = iota
	terminalSuccess
	terminalFailure
)

func (k terminalKind) String() string {
	switch k {
	case terminalSuccess:
		return "success"
	case terminalFailure:
		return "failure"
	default:
		return "none"
	}
}

type terminalEvent struct {
	Kind    terminalKind
	Type    string
	Message string
}

type sseDecoder struct {
	maxLineBytes int
	line         []byte
	eventName    string
	dataLines    []string
}

func newSSEDecoder(maxLineBytes int) *sseDecoder {
	return &sseDecoder{maxLineBytes: maxLineBytes}
}

func (d *sseDecoder) Feed(raw []byte) (terminalEvent, error) {
	for len(raw) > 0 {
		index := bytes.IndexByte(raw, '\n')
		if index < 0 {
			if err := d.appendLine(raw); err != nil {
				return terminalEvent{}, err
			}
			return terminalEvent{}, nil
		}
		if err := d.appendLine(raw[:index]); err != nil {
			return terminalEvent{}, err
		}
		event, err := d.finishLine()
		if err != nil || event.Kind != terminalNone {
			return event, err
		}
		raw = raw[index+1:]
	}
	return terminalEvent{}, nil
}

func (d *sseDecoder) Finish() (terminalEvent, error) {
	if len(d.line) > 0 {
		event, err := d.finishLine()
		if err != nil || event.Kind != terminalNone {
			return event, err
		}
	}
	return d.finishEvent(), nil
}

func (d *sseDecoder) appendLine(fragment []byte) error {
	if len(d.line)+len(fragment) > d.maxLineBytes {
		return fmt.Errorf("SSE line exceeds %d bytes", d.maxLineBytes)
	}
	d.line = append(d.line, fragment...)
	return nil
}

func (d *sseDecoder) finishLine() (terminalEvent, error) {
	line := d.line
	d.line = nil
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		return d.finishEvent(), nil
	}
	if line[0] == ':' {
		return terminalEvent{}, nil
	}

	field, value, found := bytes.Cut(line, []byte{':'})
	if !found {
		field = line
		value = nil
	}
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(field) {
	case "event":
		d.eventName = strings.TrimSpace(string(value))
	case "data":
		d.dataLines = append(d.dataLines, string(value))
	}
	return terminalEvent{}, nil
}

func (d *sseDecoder) finishEvent() terminalEvent {
	eventName := strings.TrimSpace(d.eventName)
	data := strings.Join(d.dataLines, "\n")
	d.eventName = ""
	d.dataLines = d.dataLines[:0]

	if eventName == "" && strings.TrimSpace(data) == "" {
		return terminalEvent{}
	}
	return classifySSEEvent(eventName, data)
}

func classifySSEEvent(eventName, data string) terminalEvent {
	trimmedData := strings.TrimSpace(data)

	payloadType := ""
	message := ""
	hasError := false
	if trimmedData != "" {
		var payload map[string]any
		if json.Unmarshal([]byte(trimmedData), &payload) == nil {
			payloadType = stringValue(payload["type"])
			message = firstString(
				pathString(payload, "response", "error", "message"),
				pathString(payload, "error", "message"),
				stringValue(payload["message"]),
			)
			_, hasError = payload["error"]
		}
	}

	eventType := strings.ToLower(strings.TrimSpace(firstString(payloadType, eventName)))
	switch eventType {
	case "response.completed", "message_stop":
		return terminalEvent{Kind: terminalSuccess, Type: eventType}
	case "response.failed", "response.incomplete", "response.cancelled", "error":
		return terminalEvent{Kind: terminalFailure, Type: eventType, Message: message}
	}
	if hasError {
		return terminalEvent{Kind: terminalFailure, Type: "error", Message: message}
	}
	return terminalEvent{}
}

func pathString(value map[string]any, path ...string) string {
	var current any = value
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = object[key]
		if !ok {
			return ""
		}
	}
	return stringValue(current)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
