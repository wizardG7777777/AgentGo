package llm

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3/option"
)

// sseWitness 保留有限的行前缀以识别终止标记；正文仍只由协议解码器解释。
type sseWitness struct {
	io.ReadCloser
	line          []byte
	lineTooLong   bool
	eventBytes    int64
	maxEventBytes int64
	dataLines     int
	doneCandidate bool
	done          bool
}

func (w *sseWitness) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	for _, b := range p[:n] {
		w.eventBytes++
		if w.eventBytes > w.maxEventBytes {
			return 0, NewFailure(FailureOutputLimitExceeded, PhaseStreamReceive, OriginProtocol, fmt.Errorf("SSE 事件超过冻结协议字节预算"))
		}
		if b != '\n' {
			if len(w.line) < 32 {
				w.line = append(w.line, b)
			} else {
				w.lineTooLong = true
			}
			continue
		}
		line := strings.TrimSuffix(string(w.line), "\r")
		if !w.lineTooLong && line == "" {
			if w.doneCandidate && w.dataLines == 1 {
				w.done = true
			}
			w.eventBytes = 0
			w.dataLines = 0
			w.doneCandidate = false
		} else if strings.HasPrefix(line, "data:") {
			w.dataLines++
			w.doneCandidate = !w.lineTooLong && w.dataLines == 1 && strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ") == "[DONE]"
		}
		w.line = w.line[:0]
		w.lineTooLong = false
	}
	return n, err
}

func streamWitness(budget OutputBudget) (*sseWitness, option.RequestOption) {
	w := &sseWitness{maxEventBytes: budget.MaxResponseBytes*8 + (16 << 10)}
	return w, option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		response, err := next(req)
		if err != nil {
			return response, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return response, nil
		}
		media, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if parseErr != nil || !strings.EqualFold(media, "text/event-stream") {
			_ = response.Body.Close()
			return nil, NewFailure(FailureProtocolIncompatible, PhaseResponseHeaders, OriginProtocol, fmt.Errorf("模型接口必须返回 text/event-stream"))
		}
		w.ReadCloser = response.Body
		response.Body = w
		return response, nil
	})
}
