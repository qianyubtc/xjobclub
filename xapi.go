package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// xUser 推文作者。
type xUser struct {
	ID     string
	Name   string
	Handle string
	Avatar string
}

// Tweet 抓到的推文（只保留验证需要的字段）。
type Tweet struct {
	ID        string
	Text      string // 展开 t.co、去掉尾部媒体链接后的正文
	CreatedMs int64
	User      xUser
	IsReply   bool
	InReplyTo string // 直接回复的那条推文 ID
	QuotedID  string
	Edited    bool
	Raw       []byte
}

// fetchErr 抓取错误：Retry=true 表示瞬时故障（网络/限流/5xx），可稍后重试；false 表示确定读不到（删除/保护/不存在）。
type fetchErr struct {
	Msg   string
	Retry bool
}

func (e *fetchErr) Error() string { return e.Msg }

func isRetryable(err error) bool {
	var fe *fetchErr
	return errors.As(err, &fe) && fe.Retry
}

var xHC = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type syndTweet struct {
	Typename  string `json:"__typename"`
	IDStr     string `json:"id_str"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
	Range     []int  `json:"display_text_range"`
	User      struct {
		IDStr      string `json:"id_str"`
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
		Avatar     string `json:"profile_image_url_https"`
	} `json:"user"`
	Entities struct {
		URLs []struct {
			URL      string `json:"url"`
			Expanded string `json:"expanded_url"`
			Indices  []int  `json:"indices"`
		} `json:"urls"`
		Media []struct {
			URL string `json:"url"`
		} `json:"media"`
	} `json:"entities"`
	Parent            *json.RawMessage `json:"parent"`
	InReplyToStatusID string           `json:"in_reply_to_status_id_str"`
	InReplyToScreen   string           `json:"in_reply_to_screen_name"`
	Quoted            *struct {
		IDStr string `json:"id_str"`
	} `json:"quoted_tweet"`
	IsEdited    bool `json:"isEdited"`
	EditControl struct {
		EditTweetIDs []string `json:"edit_tweet_ids"`
	} `json:"edit_control"`
}

// fetchTweet 用 X 嵌入组件的公开接口读一条推文（免 API Key）。结果写入 tweet_cache。
func (a *App) fetchTweet(id, purpose string) (*Tweet, error) {
	a.fetchSem <- struct{}{}
	defer func() { <-a.fetchSem }()
	tw, err := a.fetchTweetRaw(id)
	ok, msg, payload := int64(1), "", ""
	if err != nil {
		ok, msg = 0, err.Error()
	} else {
		payload = string(tw.Raw)
	}
	a.st.db.Exec(`INSERT INTO tweet_cache(tweet_id,purpose,ok,err,payload,fetched_at) VALUES(?,?,?,?,?,?)`, id, purpose, ok, msg, payload, ms())
	return tw, err
}

func (a *App) fetchTweetRaw(id string) (*Tweet, error) {
	req, _ := http.NewRequest("GET", a.cfg.XTweetAPI+"/tweet-result?id="+id+"&token="+tweetToken(id), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (xjobclub)")
	resp, err := xHC.Do(req)
	if err != nil {
		return nil, &fetchErr{Msg: "连不上 X 接口", Retry: true}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	switch {
	case resp.StatusCode == 404:
		return nil, &fetchErr{Msg: "推文不存在或已删除"}
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return nil, &fetchErr{Msg: fmt.Sprintf("X 接口暂时不可用（HTTP %d）", resp.StatusCode), Retry: true}
	case resp.StatusCode != 200:
		return nil, &fetchErr{Msg: fmt.Sprintf("X 接口返回 HTTP %d", resp.StatusCode), Retry: resp.StatusCode != 400 && resp.StatusCode != 403}
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, &fetchErr{Msg: "推文读不到（可能已删除或账号受保护）"}
	}
	var t syndTweet
	if err := json.Unmarshal(body, &t); err != nil || t.User.ScreenName == "" {
		if t.Typename == "TweetTombstone" {
			return nil, &fetchErr{Msg: "推文已不可见（已删除、受保护或被 X 限制）"}
		}
		return nil, &fetchErr{Msg: "推文读不到（可能已删除或账号受保护）"}
	}
	tw := &Tweet{ID: id, Raw: body, Edited: t.IsEdited || len(t.EditControl.EditTweetIDs) > 1}
	if t.IDStr != "" {
		tw.ID = t.IDStr
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.RubyDate} {
		if ts, err := time.Parse(layout, t.CreatedAt); err == nil {
			tw.CreatedMs = ts.UnixMilli()
			break
		}
	}
	tw.User = xUser{ID: t.User.IDStr, Name: t.User.Name, Handle: t.User.ScreenName, Avatar: t.User.Avatar}
	tw.IsReply = t.Parent != nil || t.InReplyToStatusID != "" || t.InReplyToScreen != ""
	tw.InReplyTo = t.InReplyToStatusID
	if tw.InReplyTo == "" && t.Parent != nil {
		var pt struct {
			IDStr string `json:"id_str"`
		}
		json.Unmarshal(*t.Parent, &pt)
		tw.InReplyTo = pt.IDStr
	}
	if t.Quoted != nil {
		tw.QuotedID = t.Quoted.IDStr
	}
	text := t.Text
	if len(t.Range) == 2 && t.Range[0] >= 0 && t.Range[1] >= t.Range[0] {
		if r := []rune(text); t.Range[1] <= len(r) {
			text = string(r[t.Range[0]:t.Range[1]])
		}
	}
	for _, m := range t.Entities.Media {
		text = strings.ReplaceAll(text, m.URL, "")
	}
	for _, u := range t.Entities.URLs {
		if u.URL != "" && u.Expanded != "" {
			text = strings.ReplaceAll(text, u.URL, u.Expanded)
		}
	}
	tw.Text = strings.TrimSpace(text)
	return tw, nil
}

var reFollowers = regexp.MustCompile(`"followers_count":(\d+)`)

// fetchFollowers 读取公开粉丝数：先试 X 嵌入时间线页（官方接口，返回 HTML 内嵌 JSON），再退回 FxTwitter 风格镜像。
// 两者都免 Key；任何失败返回 error，调用方把粉丝数视为"未知"。
func (a *App) fetchFollowers(handle string) (int64, error) {
	a.fetchSem <- struct{}{}
	defer func() { <-a.fetchSem }()
	if a.cfg.XSyndAPI != "" {
		req, _ := http.NewRequest("GET", a.cfg.XSyndAPI+"/srv/timeline-profile/screen-name/"+handle, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (xjobclub)")
		if resp, err := xHC.Do(req); err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				if m := reFollowers.FindSubmatch(body); m != nil {
					if n, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
						return n, nil
					}
				}
			}
		}
	}
	if a.cfg.XProfileAPI == "" {
		return -1, errors.New("no profile source")
	}
	req, _ := http.NewRequest("GET", a.cfg.XProfileAPI+"/"+handle, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (xjobclub)")
	req.Header.Set("Accept", "application/json")
	resp, err := xHC.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != 200 {
		return -1, fmt.Errorf("profile api %d", resp.StatusCode)
	}
	var d struct {
		Code int `json:"code"`
		User struct {
			Followers *int64 `json:"followers"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &d); err != nil || d.User.Followers == nil {
		return -1, errors.New("profile api: no followers")
	}
	return *d.User.Followers, nil
}

// fetchRetweeted 从 X 公开的个人时间线页里找接单方是否转发过目标推文。
// 只能看到最近若干条动态，找不到不代表没转（交发布方核对）。
func (a *App) fetchRetweeted(handle, targetID string) (bool, error) {
	if !reHandleOK.MatchString(handle) {
		return false, &fetchErr{Msg: "用户名不合法"}
	}
	req, _ := http.NewRequest("GET", a.cfg.XSyndAPI+"/srv/timeline-profile/screen-name/"+handle, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36")
	resp, err := xHC.Do(req)
	if err != nil {
		return false, &fetchErr{Msg: "连不上 X 接口", Retry: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, &fetchErr{Msg: fmt.Sprintf("X 接口返回 HTTP %d", resp.StatusCode), Retry: resp.StatusCode == 429 || resp.StatusCode >= 500}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return retweetedIn(string(body), targetID), nil
}

var reHandleOK = regexp.MustCompile(`^[A-Za-z0-9_]{1,20}$`)

// retweetedIn 在时间线 JSON 文本里找 "retweeted_status":{...,"id_str":"<id>"}（同一层级的 id_str）。
func retweetedIn(s, id string) bool {
	if id == "" {
		return false
	}
	if strings.Contains(s, `"retweeted_status_id_str":"`+id+`"`) {
		return true
	}
	key := `"retweeted_status":{`
	for i := 0; ; {
		j := strings.Index(s[i:], key)
		if j < 0 {
			return false
		}
		p := i + j + len(key)
		depth := 1
		for k := p; k < len(s) && depth > 0; k++ {
			switch s[k] {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			case '"':
				if depth == 1 && strings.HasPrefix(s[k:], `"id_str":"`) {
					v := s[k+len(`"id_str":"`):]
					if e := strings.IndexByte(v, '"'); e > 0 && v[:e] == id {
						return true
					}
				}
				e := k + 1
				for e < len(s) && s[e] != '"' {
					if s[e] == '\\' {
						e++
					}
					e++
				}
				k = e
			}
		}
		i = p
	}
}
