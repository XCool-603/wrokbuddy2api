package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// Anthropic Messages API 请求结构定义
type anthropicMessageRequest struct {
	Model       string          `json:"model"`
	Messages    []anthropicMsg  `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"` // 可能是 string 或 []anthropicContentPart
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // 可能是 string 或 []map[string]any
}

// messagesHandler 处理 POST /v1/messages 端点（Anthropic Claude 原生协议适配器）
func (h *Handler) messagesHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body: "+err.Error())
		return
	}

	var req anthropicMessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	// 1. 将 Anthropic 协议转换为 OpenAI 标准 chat/completions 格式
	openAIReq := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		openAIReq["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		openAIReq["temperature"] = *req.Temperature
	}

	var messages []map[string]any

	// 提取 system 提示词
	if len(req.System) > 0 {
		var sysStr string
		if err := json.Unmarshal(req.System, &sysStr); err == nil && sysStr != "" {
			messages = append(messages, map[string]any{
				"role":    "system",
				"content": sysStr,
			})
		} else {
			var sysParts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(req.System, &sysParts); err == nil {
				var sb strings.Builder
				for _, p := range sysParts {
					if p.Type == "text" {
						sb.WriteString(p.Text)
					}
				}
				if sb.Len() > 0 {
					messages = append(messages, map[string]any{
						"role":    "system",
						"content": sb.String(),
					})
				}
			}
		}
	}

	// 转换 messages
	for _, m := range req.Messages {
		var contentStr string
		if err := json.Unmarshal(m.Content, &contentStr); err == nil {
			messages = append(messages, map[string]any{
				"role":    m.Role,
				"content": contentStr,
			})
			continue
		}

		// 结构化 content（如包含 text 或 image）
		var parts []map[string]any
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			var convParts []map[string]any
			for _, p := range parts {
				pType, _ := p["type"].(string)
				switch pType {
				case "text":
					convParts = append(convParts, map[string]any{
						"type": "text",
						"text": p["text"],
					})
				case "image":
					// Claude 格式: source: {type: "base64", media_type: "image/png", data: "..."}
					if src, ok := p["source"].(map[string]any); ok {
						data, _ := src["data"].(string)
						mediaType, _ := src["media_type"].(string)
						if mediaType == "" {
							mediaType = "image/png"
						}
						convParts = append(convParts, map[string]any{
							"type": "image_url",
							"image_url": map[string]any{
								"url": fmt.Sprintf("data:%s;base64,%s", mediaType, data),
							},
						})
					}
				}
			}
			messages = append(messages, map[string]any{
				"role":    m.Role,
				"content": convParts,
			})
		}
	}

	openAIReq["messages"] = messages
	openAIBody, err := json.Marshal(openAIReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "marshal openAI payload failed: "+err.Error())
		return
	}

	// 2. 构造下游虚拟请求并调用 chatCompletions
	newReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(openAIBody))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "create internal request failed: "+err.Error())
		return
	}
	newReq.Header = r.Header.Clone()

	// 鉴权对齐：若携带了 Anthropic 特有的 x-api-key 但没有 Authorization，则自动补齐 Bearer
	if r.Header.Get("Authorization") == "" && r.Header.Get("x-api-key") != "" {
		newReq.Header.Set("Authorization", "Bearer "+r.Header.Get("x-api-key"))
	}

	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	if !req.Stream {
		// 非流式响应转换
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, newReq)

		for k, vv := range rec.Header() {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}

		if rec.Code >= 400 {
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}

		var oResp struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &oResp); err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "unmarshal internal response failed: "+err.Error())
			return
		}

		content := ""
		if len(oResp.Choices) > 0 {
			content = oResp.Choices[0].Message.Content
		}

		stopReason := "end_turn"
		if len(oResp.Choices) > 0 && oResp.Choices[0].FinishReason == "length" {
			stopReason = "max_tokens"
		}

		claudeResp := map[string]any{
			"id":   msgID,
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{
					"type": "text",
					"text": content,
				},
			},
			"model":         req.Model,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  oResp.Usage.PromptTokens,
				"output_tokens": oResp.Usage.CompletionTokens,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(claudeResp)
		return
	}

	// 3. 流式响应转换为 Anthropic SSE 事件流
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, _ := w.(http.Flusher)

	// 使用 pipe 连接内部流式写入与转换器
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		cw := &pipeResponseWriter{
			pipeWriter: pw,
			header:     make(http.Header),
		}
		h.chatCompletions(cw, newReq)
	}()

	scanner := bufio.NewScanner(pr)
	var outputTokens int
	started := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		dataStr := strings.TrimPrefix(line, "data: ")
		if strings.TrimSpace(dataStr) == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
			continue
		}

		if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
			outputTokens = chunk.Usage.CompletionTokens
		}

		// 首次下发 message_start 与 content_block_start
		if !started {
			started = true
			msgStart, _ := json.Marshal(map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            msgID,
					"type":          "message",
					"role":          "assistant",
					"content":       []any{},
					"model":         req.Model,
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage": map[string]any{
						"input_tokens":  0,
						"output_tokens": 1,
					},
				},
			})
			fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", msgStart)

			cbStart, _ := json.Marshal(map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", cbStart)
			if flusher != nil {
				flusher.Flush()
			}
		}

		if len(chunk.Choices) > 0 {
			text := chunk.Choices[0].Delta.Content
			if text != "" {
				outputTokens++
				deltaPayload, _ := json.Marshal(map[string]any{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]any{
						"type": "text_delta",
						"text": text,
					},
				})
				fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", deltaPayload)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}

	if started {
		// content_block_stop
		cbStop, _ := json.Marshal(map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		})
		fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", cbStop)

		// message_delta
		msgDelta, _ := json.Marshal(map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"output_tokens": outputTokens,
			},
		})
		fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", msgDelta)

		// message_stop
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// pipeResponseWriter 适配器，用于让 chatCompletions 输出写入 Pipe
type pipeResponseWriter struct {
	pipeWriter *io.PipeWriter
	header     http.Header
	statusCode int
}

func (p *pipeResponseWriter) Header() http.Header {
	return p.header
}

func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	return p.pipeWriter.Write(b)
}

func (p *pipeResponseWriter) WriteHeader(statusCode int) {
	p.statusCode = statusCode
}

func (p *pipeResponseWriter) Flush() {}
