// responses_stream.go OpenAI Responses API 流式（SSE）转换：Chat chunk → Responses 事件。
//
// 官方事件序列（简化版，覆盖 message 与 function_call 两种 output）：
//
//	response.created
//	response.in_progress
//	[文本路径]
//	  response.output_item.added        (item.type=message)
//	  response.content_part.added       (part.type=output_text)
//	  response.output_text.delta        ×N
//	  response.output_text.done
//	  response.content_part.done
//	  response.output_item.done
//	[工具路径]
//	  response.output_item.added        (item.type=function_call)
//	  response.function_call_arguments.delta ×N
//	  response.function_call_arguments.done
//	  response.output_item.done
//	response.completed                  （status=completed | incomplete）
//	data: [DONE]
//
// 关键设计：不存在服务端会话。工具调用在本次响应内一次性输出完成后即结束流，
// 客户端下一轮把 function_call_output 放进 input 重新请求（无状态，可水平扩展）。
package gateway

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// streamResponses 把内层 Chat SSE 转成 Responses SSE 写回客户端。
func (g *Gateway) streamResponses(w http.ResponseWriter, r *http.Request, res *innerResult, clientModel string, req chatRequest) {
	defer res.Close()

	// 内层已回错误状态：按 JSON 错误体返回，而非 SSE（客户端无需处理流内错误）
	if res.Status >= 400 {
		raw, _ := res.ReadAll()
		writeUpstreamError(w, res.Status, raw, false)
		return
	}

	flush := sseHeader(w)
	respID := "resp_" + randSuffix()
	createdAt := nowUnix()
	base := map[string]any{
		"id": respID, "object": "response", "created_at": createdAt,
		"model": clientModel, "status": "in_progress", "output": []any{},
		"parallel_tool_calls": true, "tool_choice": "auto",
	}

	var (
		seq        int
		text       strings.Builder
		tools      = newToolCallAccumulator()
		finish     string
		usage      map[string]any
		msgItemID  string
		partOpen   bool
		msgEmitted bool
	)
	emit := func(event string, payload map[string]any) error {
		payload["sequence_number"] = seq
		seq++
		return writeSSEEvent(w, flush, event, payload)
	}

	if err := emit("response.created", map[string]any{"type": "response.created", "response": base}); err != nil {
		return
	}
	if err := emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": base}); err != nil {
		return
	}

	// ensureMessageItem 惰性开启文本 output item：只有真正来文本才发，避免空 message item。
	ensureMessageItem := func() error {
		if msgEmitted {
			return nil
		}
		msgItemID = "msg_" + randSuffix()
		if err := emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{
				"id": msgItemID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		}); err != nil {
			return err
		}
		msgEmitted = true
		return nil
	}
	ensureContentPart := func() error {
		if partOpen || !msgEmitted {
			return nil
		}
		if err := emit("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": msgItemID,
			"output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}},
		}); err != nil {
			return err
		}
		partOpen = true
		return nil
	}

	err := iterateChatSSE(res.Body, func(c chatChunk) error {
		// 流内错误帧：直接中断（修复前被 parseChatSSELine 丢弃 → 走到正常收尾，
		// 客户端看到 response.completed + [DONE]，把半截回答当完整回答）。
		if c.Err != nil {
			return c.Err
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if c.Content != "" {
			if err := ensureMessageItem(); err != nil {
				return err
			}
			if err := ensureContentPart(); err != nil {
				return err
			}
			if err := emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": msgItemID,
				"output_index": 0, "content_index": 0, "delta": c.Content, "logprobs": []any{},
			}); err != nil {
				return err
			}
			text.WriteString(c.Content)
		}
		// 工具调用在流结束后统一输出：需先收齐 arguments 才能构造完整 item。
		for _, tc := range c.ToolCalls {
			tools.Add(tc)
		}
		return nil
	})
	if err != nil {
		if err != io.EOF {
			// 自由码：流内错误（*StreamError）带内层私有码；IO/读错误没有码，
			// 必须兜底为非空串——否则真实中断场景（内层写到一半断开 → 读端 ErrClosedPipe）
			// 会发出 "code":""，把原来（固定 "stream_error"）的诊断信息丢掉。
			freeCode := streamErrCode(err)
			if freeCode == "" {
				freeCode = "stream_error"
			}
			_ = emit("error", map[string]any{
				"type": "error", "code": freeCode, "message": err.Error(),
				"sequence_number": seq,
			})
			// 失败终态：响应式 API 的规范做法是 response.failed，
			// **不补 response.completed、不补 [DONE]**——否则客户端把失败的流
			// 当成功收尾（半截内容视为完整回答），正是流内错误要消灭的症状。
			//
			// response.error.code 官方是 Literal 枚举（server_error / rate_limit_exceeded …），
			// 故此处**映射到枚举内**；内层私有码只放在上面 error 事件的 code 字段
			// （该字段规范上就是 Optional[str]，是自由码的合规载体）。
			errCode := "server_error"
			if streamErrKind(err) == streamErrRateLimit {
				errCode = "rate_limit_exceeded"
			}
			failedResp := map[string]any{
				"id": respID, "object": "response", "created_at": createdAt,
				"model": clientModel, "status": "failed", "output": []any{},
				"error": map[string]any{"code": errCode, "message": err.Error()},
			}
			_ = emit("response.failed", map[string]any{"type": "response.failed", "response": failedResp})
			flush()
			time.Sleep(20 * time.Millisecond)
			return
		}
	}

	// ---- 收尾：按官方顺序关闭已开启的文本块，再输出工具调用 ----
	output := make([]any, 0, 2)
	toolCalls := tools.List()

	if msgEmitted {
		full := text.String()
		if err := emit("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": msgItemID,
			"output_index": 0, "content_index": 0, "text": full, "logprobs": []any{},
		}); err != nil {
			return
		}
		if partOpen {
			if err := emit("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "item_id": msgItemID,
				"output_index": 0, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}, "logprobs": []any{}},
			}); err != nil {
				return
			}
		}
		item := messageItem(full)
		item["id"] = msgItemID
		if err := emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": item,
		}); err != nil {
			return
		}
		output = append(output, item)
	}

	// 工具调用：每个调用是独立 output item（index 从文本 item 之后开始）
	for _, tc := range toolCalls {
		idx := len(output)
		args := tc.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		itemID := "fc_" + randSuffix()
		callID := tc.ID
		if strings.TrimSpace(callID) == "" {
			callID = "call_" + randSuffix()
		}
		if err := emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": idx,
			"item": map[string]any{
				"id": itemID, "type": "function_call", "status": "in_progress",
				"call_id": callID, "name": tc.Name, "arguments": "",
			},
		}); err != nil {
			return
		}
		if args != "" {
			if err := emit("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": itemID,
				"output_index": idx, "delta": args,
			}); err != nil {
				return
			}
		}
		if err := emit("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": itemID,
			"output_index": idx, "arguments": args,
		}); err != nil {
			return
		}
		item := map[string]any{
			"id": itemID, "type": "function_call", "status": "completed",
			"call_id": callID, "name": tc.Name, "arguments": args,
		}
		if err := emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": idx, "item": item,
		}); err != nil {
			return
		}
		output = append(output, item)
	}

	// ---- response.completed ----
	// 规范说明：模型产生 function_call 时响应本身仍是 completed，
	// 客户端从 output 里看到 function_call 后自行发起下一轮请求。
	// （incomplete_details.reason 只允许 max_output_tokens / content_filter）
	final := map[string]any{
		"id": respID, "object": "response", "created_at": createdAt,
		"model": clientModel, "status": "completed", "output": output, "output_text": text.String(),
		"parallel_tool_calls": true, "tool_choice": "auto",
	}
	// finish_reason=length 表示输出被截断（与 tool_calls 互斥，无需额外判定）
	if strings.EqualFold(finish, "length") {
		final["status"] = "incomplete"
		final["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if u := responsesUsage(usage); u != nil {
		final["usage"] = u
	}
	_ = emit("response.completed", map[string]any{"type": "response.completed", "response": final})

	// 收尾留一点时间让客户端处理完最后一个事件（同 tokligence 做法，避免连接过早关闭）
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flush()
	time.Sleep(20 * time.Millisecond)
}
