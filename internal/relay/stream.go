package relay

import (
	"context"
	"io"
	"strings"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
	"github.com/ZFXing-lite/orcaterm2api/internal/keypool"
	"github.com/ZFXing-lite/orcaterm2api/internal/orca"
)

// toHTTPErr relay 内部错误→HTTPError（server 包的 errToHTTP 同语义，避免反向依赖）。
func toHTTPErr(err error) *HTTPError {
	if he, ok := err.(*HTTPError); ok {
		return he
	}
	return &HTTPError{Status: 500, Code: "internal_error", Message: err.Error()}
}

// StreamEvents 下游流式事件回调：OnText 逐段下发可见文本；
// 结束时恰一次调用 OnFinish（成功）或 OnError（失败）。
type StreamEvents struct {
	OnText   func(piece string) error
	OnFinish func(resp *ChatResponse) error
	OnError  func(err *HTTPError)
}

// streamHandle 已建立的上游流：establish 阶段缓存的片段先发，
// 之后从 TokenStream 继续消费直至 EOF。
type streamHandle struct {
	ts      *orca.TokenStream
	ext     *orca.StreamFieldExtractor
	pending []string // establish 阶段已提取但未下发的片段
}

// CompleteStream 真流式补全：上游 SSE 直通到下游（打字机），
// 流末聚合协议 JSON 重建完整响应（工具调用/usage/finish reason）。
// 下游收到首段可见文本之前，上游失败仍按既有语义换号重试。
func (e *Engine) CompleteStream(ctx context.Context, req *ChatRequest, sessionKey string, ev StreamEvents) {
	snap := e.cfg.Snapshot()

	// 模型解析（与 Complete 同规则）
	modelName := req.Model
	if snap.Upstream.ForceModel || modelName == "" {
		modelName = snap.Upstream.DefaultModel
	}
	upstreamModel, err := orca.ResolveModel(modelName, defaultUpstreamModel(snap), snap.Upstream.AllowUnknownModel)
	if err != nil {
		ev.OnError(&HTTPError{Status: 404, Code: "model_not_found", Message: err.Error()})
		return
	}
	if err := validateTools(req); err != nil {
		if he, ok := err.(*HTTPError); ok {
			ev.OnError(he)
		} else {
			ev.OnError(&HTTPError{Status: 400, Code: "invalid_request", Message: err.Error()})
		}
		return
	}
	unsupported := unsupportedTools(req.Tools)

	// 工具结果回填：聚合语义，流式无意义
	if results := e.extractToolResults(req); len(results) > 0 {
		resp, err := e.continueToolTurn(ctx, snap, req, results, sessionKey, unsupported)
		if err != nil {
			ev.OnError(toHTTPErr(err))
			return
		}
		if len(resp.Text) > 0 {
			if err := ev.OnText(resp.Text); err != nil {
				return
			}
		}
		ev.OnFinish(resp)
		return
	}

	// 输入组装按路径进行：pooled=独立轮；session=按 isNew（与聚合路径同语义）
	if sessionKey != "" {
		e.sessionStream(ctx, snap, req, sessionKey, modelName, upstreamModel, unsupported, ev)
		return
	}
	e.pooledStream(ctx, snap, req, modelName, upstreamModel, unsupported, ev)
}

// buildOreq 构造上游请求（text/images 由调用方按路径语义决定）。
func (e *Engine) buildOreq(snap config.Config, req *ChatRequest, text string, images []orca.ImagePart, upstreamModel string, convID string) (orca.ChatRequest, error) {
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return orca.ChatRequest{}, &HTTPError{Status: 400, Code: "empty_input", Message: "消息内容为空"}
	}
	return orca.ChatRequest{
		ConversationID: convID,
		Text:           text,
		Images:         images,
		UpstreamModel:  upstreamModel,
		HideTools:      snap.Upstream.HideTools,
		EnableTools:    len(req.Tools) > 0,
	}, nil
}

// establish 建立流并推进到首个可见文本（或 EOF）：
// 此前失败对调用方可重试；成功后句柄持有流供下发循环消费。
func establish(ctx context.Context, client *orca.Client, acct *keypool.Account, oreq orca.ChatRequest) (*streamHandle, error) {
	ts, err := client.ChatStreamCollect(ctx, acct.Credential(), oreq)
	if err != nil {
		return nil, err
	}
	h := &streamHandle{ts: ts, ext: orca.NewStreamFieldExtractor()}
	for {
		tok, err := ts.Next()
		if err != nil {
			// EOF：review 等无可见文本场景属正常（流末聚合兜底）
			if isEOFErr(err) {
				return h, nil
			}
			return nil, err
		}
		if piece := h.ext.Feed(tok); piece != "" {
			h.pending = append(h.pending, piece)
			return h, nil
		}
	}
}

// pump 消费流句柄：下发 establish 缓存 + 持续提取直至 EOF。
// 返回聚合的协议 JSON 全文（供 parseOutcome 重建完整响应）。
func (h *streamHandle) pump(ev StreamEvents) error {
	for _, p := range h.pending {
		if err := ev.OnText(p); err != nil {
			return err
		}
	}
	h.pending = nil
	for {
		tok, err := h.ts.Next()
		if err != nil {
			if isEOFErr(err) {
				return nil
			}
			return err
		}
		if piece := h.ext.Feed(tok); piece != "" {
			if err := ev.OnText(piece); err != nil {
				return err
			}
		}
	}
}

func isEOFErr(err error) bool { return err == io.EOF }

// pooledStream 无会话流式路径：跨号重试窗口 = establish 成功之前。
func (e *Engine) pooledStream(ctx context.Context, snap config.Config, req *ChatRequest, modelName, upstreamModel string, unsupported []string, ev StreamEvents) {
	text, images := buildTurnInput(req, true)
	convID := orca.NewConversationID(snap.Upstream.Product)
	oreq, berr := e.buildOreq(snap, req, text, images, upstreamModel, convID)
	if berr != nil {
		ev.OnError(toHTTPErr(berr))
		return
	}
	res, acct, err := e.pool.DoWithRetry(ctx, func(a *keypool.Account) (any, bool, int, error) {
		h, herr := establish(ctx, e.clientFor(a), a, oreq)
		if herr != nil {
			retryable, status, herr2 := e.retryableOf(herr)
			if isAuthError(herr) {
				e.pool.ReportFailure(a, keypool.FailAuthExpired, herr.Error(), 0)
			} else if status == 429 {
				e.pool.ReportFailure(a, keypool.FailRateLimited, herr.Error(), 0)
			} else {
				e.pool.ReportFailure(a, keypool.FailServerError, herr.Error(), 0)
			}
			return nil, retryable, status, herr2
		}
		return h, false, 200, nil
	})
	if err != nil {
		ev.OnError(toHTTPErr(err))
		return
	}
	h := res.(*streamHandle)
	e.finishStream(ctx, snap, req, h, acct, acct.ID, modelName, upstreamModel, "", convID, unsupported, text, ev)
}

// sessionStream 会话粘性流式路径：固定账号，失败不换号。
func (e *Engine) sessionStream(ctx context.Context, snap config.Config, req *ChatRequest, sessionKey string, modelName, upstreamModel string, unsupported []string, ev StreamEvents) {
	convID, acct, isNew := e.resolveSession(sessionKey)
	if acct == nil {
		ev.OnError(&HTTPError{Status: 503, Code: "no_upstream_account", Message: "无可用上游账号"})
		return
	}
	defer e.pool.Release(acct)
	text, images := buildTurnInput(req, isNew)
	oreq, berr := e.buildOreq(snap, req, text, images, upstreamModel, convID)
	if berr != nil {
		ev.OnError(toHTTPErr(berr))
		return
	}
	h, err := establish(ctx, e.clientFor(acct), acct, oreq)
	if err != nil {
		ev.OnError(toHTTPErr(err))
		return
	}
	e.finishStream(ctx, snap, req, h, acct, acct.ID, modelName, upstreamModel, sessionKey, convID, unsupported, text, ev)
}

// finishStream 下发流 + 流末聚合重建完整响应。
func (e *Engine) finishStream(ctx context.Context, snap config.Config, req *ChatRequest, h *streamHandle, acct *keypool.Account, accountID, modelName, upstreamModel, sessionKey, convID string, unsupported []string, promptText string, ev StreamEvents) {
	perr := h.pump(ev)
	// 无论下发结果如何，聚合全文都已尽力收集
	full := h.ts.Full()
	outcome, oerr := parseOutcomeSafe(full)
	if oerr != nil {
		// 协议层错误：若已下发过文本则尽量体面收尾，否则报错
		if h.ext.Emitted() {
			ev.OnError(&HTTPError{Status: 502, Code: "upstream_protocol_error", Message: oerr.Error()})
			return
		}
		ev.OnError(toHTTPErr(oerr))
		return
	}
	if perr != nil {
		// 下游断开：账务照记，不下发收尾
		e.pool.ReportSuccess(acct)
		return
	}
	e.pool.ReportSuccess(acct)
	resp := e.buildResponse(req, snap, outcome, accountID, upstreamModel, sessionKey, convID, unsupported, promptText)
	e.track(accountID, modelName, resp.Usage, nil)
	if err := ev.OnFinish(resp); err != nil {
		// 下游已断
	}
}

func parseOutcomeSafe(full string) (*orca.Outcome, error) {
	if strings.TrimSpace(full) == "" {
		return nil, &HTTPError{Status: 502, Code: "upstream_empty", Message: "上游未返回内容"}
	}
	return orca.ParseOutcomeText(full)
}
