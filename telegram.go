package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Telegram 通知机器人。
// 用户在「设置」里点绑定 → 生成一次性绑定码 → 打开 t.me/<bot>?start=<码> → 机器人收到 /start <码> 把这个聊天绑到用户 →
// 之后每条站内通知同步推送到 Telegram。用 getUpdates 长轮询，不需要公网回调地址；令牌只存在配置文件里。

type tgBot struct {
	a     *App
	token string
	base  string // API 根（测试用桩）
	mu    sync.RWMutex
	name  string // 机器人用户名（getMe 拿到，深链用；poller 写、请求线程读）
	queue chan tgMsg
	hc    *http.Client
}

func (b *tgBot) botName() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.name
}

type tgMsg struct {
	chatID int64
	text   string
}

// TGLink 用户与 Telegram 聊天的绑定。
type TGLink struct {
	UserID    int64
	ChatID    int64
	Username  string
	CreatedAt int64
}

func newTGBot(a *App) *tgBot {
	if strings.TrimSpace(a.cfg.TGBotToken) == "" {
		return nil
	}
	return &tgBot{a: a, token: strings.TrimSpace(a.cfg.TGBotToken), base: strings.TrimRight(a.cfg.TGAPIBase, "/"), queue: make(chan tgMsg, 1000), hc: &http.Client{Timeout: 40 * time.Second}}
}

// startTG 启动发送与轮询协程（服务模式才启动；维护命令不启动）。
func (a *App) startTG() {
	if a.tg == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.wg.Add(3)
	go func() { defer a.wg.Done(); <-a.stop; cancel() }()
	go func() { defer a.wg.Done(); a.tg.sender(ctx) }()
	go func() { defer a.wg.Done(); a.tg.poller(ctx) }()
}

func (a *App) tgName() string {
	if a.tg == nil {
		return ""
	}
	return a.tg.botName()
}

// call 调一个 Bot API 方法。返回 result 原文；429 时带上 retry_after。
func (b *tgBot) call(ctx context.Context, method string, params url.Values) (json.RawMessage, int, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", b.base+"/bot"+b.token+"/"+method, strings.NewReader(params.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // *url.Error 带完整 URL（含令牌），日志里只留底层原因
		}
		return nil, 0, fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, 0, fmt.Errorf("telegram %s: http %d", method, resp.StatusCode)
	}
	if !out.OK {
		return nil, out.Parameters.RetryAfter, fmt.Errorf("telegram %s: %s", method, strings.TrimSpace(out.Description))
	}
	return out.Result, 0, nil
}

// send 入队（不阻塞请求线程）。
func (b *tgBot) send(chatID int64, text string) {
	if chatID == 0 || text == "" {
		return
	}
	select {
	case b.queue <- tgMsg{chatID: chatID, text: text}:
	default:
		log.Printf("[warn] telegram 发送队列已满，丢弃一条")
	}
}

func (b *tgBot) sender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			b.drain(5 * time.Second)
			return
		case m := <-b.queue:
			b.deliver(ctx, m)
		}
	}
}

// deliver 发一条：429 按 retry_after 重试一次；被拉黑 / 聊天不存在则解绑。
func (b *tgBot) deliver(ctx context.Context, m tgMsg) {
	for attempt := 0; attempt < 2; attempt++ {
		_, retry, err := b.call(ctx, "sendMessage", url.Values{"chat_id": {strconv.FormatInt(m.chatID, 10)}, "text": {m.text}, "parse_mode": {"HTML"}, "disable_web_page_preview": {"true"}})
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		msg := err.Error()
		if strings.Contains(msg, "blocked") || strings.Contains(msg, "chat not found") || strings.Contains(msg, "deactivated") {
			b.a.st.TGUnbindChat(m.chatID) // 用户拉黑了机器人：解绑，别再打
			break
		}
		if retry > 0 && attempt == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(retry) * time.Second):
			}
			continue
		}
		log.Printf("[warn] telegram 发送失败: %v", err)
		break
	}
	time.Sleep(40 * time.Millisecond) // 全局 30 条/秒以内
}

// drain 把队列里剩下的同步发完（停机 / 维护命令退出前用），最多花 limit。
func (b *tgBot) drain(limit time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	for {
		select {
		case m := <-b.queue:
			b.deliver(ctx, m)
			if ctx.Err() != nil {
				return
			}
		default:
			return
		}
	}
}

// flushTG 维护命令退出前把攒下的推送发出去。
func (a *App) flushTG() {
	if a.tg != nil {
		a.tg.drain(5 * time.Second)
	}
}

// poller 长轮询收命令：/start <绑定码>、/stop、其它给提示。
func (b *tgBot) poller(ctx context.Context) {
	for b.botName() == "" {
		raw, _, err := b.call(ctx, "getMe", nil)
		if err == nil {
			var me struct {
				Username string `json:"username"`
			}
			json.Unmarshal(raw, &me)
			if me.Username == "" {
				err = errors.New("getMe 没有返回 username")
			} else {
				b.mu.Lock()
				b.name = me.Username
				b.mu.Unlock()
				log.Printf("[info] telegram 机器人 @%s 已连接", me.Username)
				break
			}
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("[warn] telegram getMe: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
	offsetStr, _ := b.a.st.GetMeta("tg_offset")
	offset, _ := strconv.ParseInt(offsetStr, 10, 64)
	var lastWarn time.Time
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
			return true
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		raw, _, err := b.call(ctx, "getUpdates", url.Values{"offset": {strconv.FormatInt(offset, 10)}, "timeout": {"25"}, "allowed_updates": {`["message"]`}})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if time.Since(lastWarn) > time.Minute {
				lastWarn = time.Now()
				hint := ""
				if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "Conflict") {
					hint = "（另一个进程在用同一个令牌轮询，或曾设置过 webhook）"
				}
				log.Printf("[warn] telegram getUpdates: %v%s", err, hint)
			}
			if !wait() {
				return
			}
			continue
		}
		var updates []struct {
			ID      int64 `json:"update_id"`
			Message *struct {
				Text string `json:"text"`
				Chat struct {
					ID   int64  `json:"id"`
					Type string `json:"type"`
				} `json:"chat"`
				From struct {
					Username string `json:"username"`
				} `json:"from"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &updates); err != nil {
			if !wait() {
				return
			}
			continue
		}
		for _, u := range updates {
			if u.ID >= offset {
				offset = u.ID + 1
			}
			if u.Message == nil || u.Message.Chat.ID == 0 {
				continue
			}
			if u.Message.Chat.Type != "" && u.Message.Chat.Type != "private" {
				continue // 群 / 频道里的命令不处理：绑定只认私聊
			}
			b.handle(u.Message.Chat.ID, u.Message.From.Username, strings.TrimSpace(u.Message.Text))
		}
		if len(updates) > 0 {
			b.a.st.SetMeta("tg_offset", strconv.FormatInt(offset, 10))
		}
	}
}

func (b *tgBot) handle(chatID int64, username, text string) {
	cmd, arg, _ := strings.Cut(text, " ")
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i]
	}
	switch cmd {
	case "/start":
		code := strings.TrimSpace(arg)
		if code == "" {
			b.send(chatID, "这是"+html.EscapeString(b.a.cfg.SiteTitle)+"的通知机器人。请到网站「我的 → 设置」点「绑定 Telegram」，会自动带上绑定码。")
			return
		}
		uid, ok := b.a.st.TGConsumeCode(code)
		if !ok {
			b.send(chatID, "绑定码无效或已过期（15 分钟内有效），请回网站重新点「绑定 Telegram」。")
			return
		}
		b.a.st.TGBind(uid, chatID, username)
		u, _ := b.a.st.GetUserByID(uid)
		name := ""
		if u != nil {
			name = " @" + u.Handle
		}
		b.a.st.Audit(uid, "tg.bind", "user", uid, map[string]any{"chat": chatID}, "")
		tgn := username
		if tgn != "" {
			tgn = " @" + tgn
		}
		b.a.st.Notify(uid, "account", "已绑定 Telegram"+tgn, "以后通知会同步推送到 Telegram。不是你本人操作的话，请到「设置」里解绑。", "/me?tab=settings")
		b.send(chatID, "绑定成功"+html.EscapeString(name)+"。以后待付款、待确认到账、审核结果、申诉进展都会推到这里。发送 /stop 可解绑。")
	case "/stop":
		if l, _ := b.a.st.TGLinkByChat(chatID); l != nil {
			b.a.st.TGUnbindChat(chatID)
			b.a.st.Audit(l.UserID, "tg.unbind", "user", l.UserID, nil, "")
		}
		b.send(chatID, "已解绑，不会再推送。需要时回网站重新绑定即可。")
	default:
		b.send(chatID, "我只负责推送通知。/stop 解绑；重新绑定请到网站「我的 → 设置」。")
	}
}

// tgText 通知转成 Telegram 消息：标题加粗，正文原样，末尾带网站链接。
func (a *App) tgText(title, body, link string) string {
	var sb strings.Builder
	sb.WriteString("<b>")
	sb.WriteString(html.EscapeString(title))
	sb.WriteString("</b>")
	if strings.TrimSpace(body) != "" {
		sb.WriteString("\n")
		sb.WriteString(html.EscapeString(body))
	}
	if link != "" {
		sb.WriteString("\n")
		if strings.HasPrefix(link, "/") {
			link = strings.TrimRight(a.cfg.BaseURL, "/") + link
		}
		sb.WriteString(html.EscapeString(link))
	}
	return sb.String()
}

// tgPush 站内通知同步推送到用户绑定的 Telegram（未绑定 / 未配置机器人则什么都不做）。
func (a *App) tgPush(userID int64, title, body, link string) {
	if a.tg == nil || userID <= 0 {
		return
	}
	l, _ := a.st.TGLink(userID)
	if l == nil {
		return
	}
	a.tg.send(l.ChatID, a.tgText(title, body, link))
}

// ---- 存储 ----

func (s *Store) TGLink(userID int64) (*TGLink, error) {
	l := &TGLink{}
	err := s.db.QueryRow(`SELECT user_id,chat_id,tg_username,created_at FROM tg_links WHERE user_id=?`, userID).Scan(&l.UserID, &l.ChatID, &l.Username, &l.CreatedAt)
	if err != nil {
		return nil, nil
	}
	return l, nil
}

func (s *Store) TGLinkByChat(chatID int64) (*TGLink, error) {
	l := &TGLink{}
	err := s.db.QueryRow(`SELECT user_id,chat_id,tg_username,created_at FROM tg_links WHERE chat_id=?`, chatID).Scan(&l.UserID, &l.ChatID, &l.Username, &l.CreatedAt)
	if err != nil {
		return nil, nil
	}
	return l, nil
}

// TGBind 一个聊天只能绑一个用户，一个用户只能绑一个聊天：两边旧绑定都覆盖。
func (s *Store) TGBind(userID, chatID int64, username string) error {
	return s.tx(func(tx *sql.Tx) error {
		tx.Exec(`DELETE FROM tg_links WHERE chat_id=? OR user_id=?`, chatID, userID)
		_, err := tx.Exec(`INSERT INTO tg_links(user_id,chat_id,tg_username,created_at) VALUES(?,?,?,?)`, userID, chatID, username, ms())
		return err
	})
}

func (s *Store) TGUnbind(userID int64) { s.db.Exec(`DELETE FROM tg_links WHERE user_id=?`, userID) }

func (s *Store) TGUnbindChat(chatID int64) { s.db.Exec(`DELETE FROM tg_links WHERE chat_id=?`, chatID) }

func (s *Store) TGLinkCount() int64 { return s.count(`SELECT COUNT(*) FROM tg_links`) }

// TGNewCode 生成一次性绑定码（15 分钟有效，覆盖该用户之前的码）。
func (s *Store) TGNewCode(userID int64) (string, error) {
	code := randCode(10)
	err := s.tx(func(tx *sql.Tx) error {
		tx.Exec(`DELETE FROM tg_codes WHERE user_id=? OR created_at<?`, userID, ms()-15*60*1000)
		_, err := tx.Exec(`INSERT INTO tg_codes(code,user_id,created_at) VALUES(?,?,?)`, code, userID, ms())
		return err
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// TGConsumeCode 用掉一个绑定码，返回对应用户。
func (s *Store) TGConsumeCode(code string) (int64, bool) {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 32 {
		return 0, false
	}
	var uid, at int64
	if err := s.db.QueryRow(`DELETE FROM tg_codes WHERE code=? RETURNING user_id, created_at`, code).Scan(&uid, &at); err != nil {
		return 0, false
	}
	if ms()-at > 15*60*1000 {
		return 0, false
	}
	return uid, true
}

// TGPendingCode 用户手头还没用、没过期的绑定码（设置页展示深链和手动命令）。
func (s *Store) TGPendingCode(userID int64) string {
	var code string
	s.db.QueryRow(`SELECT code FROM tg_codes WHERE user_id=? AND created_at>? ORDER BY created_at DESC LIMIT 1`, userID, ms()-15*60*1000).Scan(&code)
	return code
}

// ---- 页面动作 ----

// handleTGBind 生成绑定码并跳到 Telegram 深链；没装 Telegram 的话页面上也会显示手动发送的命令。
func (a *App) handleTGBind(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.tg == nil || a.tg.botName() == "" {
		a.flash(w, "Telegram 通知暂未开启")
		http.Redirect(w, r, "/me?tab=settings", http.StatusFound)
		return
	}
	if a.limited(w, r, "tgbind", 10, 10*time.Minute) {
		return
	}
	if _, err := a.st.TGNewCode(u.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/me?tab=settings#tg", http.StatusFound)
}

func (a *App) handleTGUnbind(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	a.st.TGUnbind(u.ID)
	a.st.Audit(u.ID, "tg.unbind", "user", u.ID, nil, a.ip(r))
	a.flash(w, "已解绑 Telegram")
	http.Redirect(w, r, "/me?tab=settings", http.StatusFound)
}
