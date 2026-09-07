package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type streamProtocol string

const (
	streamProtocolOpenAIResponses       streamProtocol = "openai_responses"
	streamProtocolAnthropicMessages     streamProtocol = "anthropic_messages"
	streamProtocolOpenAIChatCompletions streamProtocol = "openai_chat_completions"
)

type streamProtocolSpec struct {
	Protocol streamProtocol
	Paths    []string
}

var streamProtocolSpecs = []streamProtocolSpec{
	{
		Protocol: streamProtocolOpenAIResponses,
		Paths:    []string{"/responses", "/v1/responses"},
	},
	{
		Protocol: streamProtocolAnthropicMessages,
		Paths:    []string{"/v1/messages"},
	},
	{
		Protocol: streamProtocolOpenAIChatCompletions,
		Paths:    []string{"/v1/chat/completions"},
	},
}

func streamProtocolForPath(path string) (streamProtocol, bool) {
	for _, spec := range streamProtocolSpecs {
		for _, supportedPath := range spec.Paths {
			if path == supportedPath {
				return spec.Protocol, true
			}
		}
	}
	return "", false
}

func allStreamProtocolPaths() []string {
	var paths []string
	for _, spec := range streamProtocolSpecs {
		paths = append(paths, spec.Paths...)
	}
	return paths
}

func (p streamProtocol) String() string {
	return string(p)
}

func (p streamProtocol) writeKeepalive(w http.ResponseWriter, controller *http.ResponseController, phase string) error {
	switch p {
	case streamProtocolOpenAIResponses:
		payload := struct {
			Type       string `json:"type"`
			StreamHold struct {
				Phase string `json:"phase"`
			} `json:"stream_hold"`
		}{
			Type: "response.metadata",
		}
		payload.StreamHold.Phase = phase
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
	case streamProtocolAnthropicMessages, streamProtocolOpenAIChatCompletions:
		if _, err := fmt.Fprintf(w, ": stream-hold %s\n\n", phase); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported stream protocol %q", p)
	}
	return controller.Flush()
}
