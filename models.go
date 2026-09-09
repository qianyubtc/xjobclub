package main

import "strings"

// ---- 用户 ----

type User struct {
	ID              int64
	XID             string
	Handle          string
	HandleLower     string
	DisplayName     string
	AvatarURL       string
	PassHash        string
	Status          string // active | blacklisted | banned
	RegTweetID      string
	XCreatedMs      int64
	PayerID         string // 认证付款/网关付款拿到的付款方 Pay 账户 ID
	CertPaidAt      int64
	SuspendedUntil  int64 // 留存不达标暂停接单
	JuryScore       int64
	JuryTotal       int64
	JuryAgree       int64
	JuryNoShow      int64
	JuryBannedUntil int64
	HandleStale     bool
	CreatedAt       int64
	LastLoginAt     int64
}

func (u *User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return "@" + u.Handle
}

func (u *User) XURL() string { return "https://x.com/" + u.Handle }

func (u *User) Path() string { return "/u/" + u.Handle }

func (u *User) Blacklisted() bool { return u.Status == "blacklisted" || u.Status == "banned" }

// PayProfile 收款设置。
type PayProfile struct {
	UserID       int64
	BinanceUID   string
	ReceiveEmail string
	Mode         string // manual | gateway
	BPGAccountID string
	APIKeyMasked string
	BPGLastOK    int64
	BPGLastErr   string
	UpdatedAt    int64
}

func (p *PayProfile) Gateway() bool { return p != nil && p.Mode == "gateway" && p.BPGAccountID != "" }

// ---- 任务 ----

type Task struct {
	ID             int64
	Code           string
	OwnerID        int64
	Title          string
	Contents       []string // 文案变体
	ContentsNorm   []string
	MatchMode      string // exact | contains
	RewardE8       int64
	Currency       string
	SlotsTotal     int64
	ClaimTTLMin    int64
	RetentionH     int64
	PayWindowH     int64
	DeadlineAt     int64
	MinAccountDays int64
	AdTag          bool
	Status         string // open | paused | closed
	CloseReason    string
	PausedByFreeze bool
	PublishedAt    int64
	CreatedAt      int64
	UpdatedAt      int64

	// 派生
	Used      int64 // 占用名额数
	DoneCount int64 // 已完成
	Owner     *User
}

func (t *Task) Left() int64 {
	if l := t.SlotsTotal - t.Used; l > 0 {
		return l
	}
	return 0
}

func (t *Task) Path() string { return "/t/" + t.Code }

// 接单记录状态
const (
	SClaimed  = "claimed"
	SSubmit   = "submitted"
	SVerified = "verified"
	SPayable  = "payable"
	SAwait    = "awaiting_confirm"
	SPaid     = "paid"
	SOverdue  = "overdue"
	SDisputed = "disputed"
	SVoid     = "void"
	SExpired  = "expired"
	SDefault  = "defaulted"
)

var subStatusText = map[string]string{
	SClaimed: "已接单", SSubmit: "验证中", SVerified: "已验证", SPayable: "待付款", SAwait: "待确认到账",
	SPaid: "已完成", SOverdue: "逾期未付", SDisputed: "申诉中", SVoid: "作废", SExpired: "未按时提交", SDefault: "违约",
}

func subStatus(s string) string {
	if v, ok := subStatusText[s]; ok {
		return v
	}
	return s
}

// terminalSub 是否终态。
func terminalSub(s string) bool {
	return s == SPaid || s == SVoid || s == SExpired || s == SDefault
}

// slotFree 该状态的记录是否不占名额（过期、作废释放名额）。
func slotFree(s string) bool { return s == SExpired || s == SVoid }

type Submission struct {
	ID             int64
	Code           string
	TaskID         int64
	WorkerID       int64
	VariantIdx     int64
	Status         string
	PrevStatus     string // 进入 disputed 前的状态
	ClaimedAt      int64
	ClaimExpiresAt int64
	TweetID        string
	TweetURL       string
	TweetText      string
	TweetCreatedAt int64
	VerifyAttempts int64
	VerifyRetries  int64
	LastError      string
	NextVerifyAt   int64
	VerifiedAt     int64
	RecheckDueAt   int64
	RecheckFlag    string
	RecheckTries   int64
	PayableAt      int64
	PayDeadlineAt  int64
	OverdueAt      int64
	ReportedAt     int64
	GraceUntil     int64
	MarkedPaidAt   int64
	MarkedOrderID  string
	MarkedNote     string
	UnderpaidE8    int64 // 网关少付：实付金额（0 = 无少付）
	TopupRequested int64 // 接单方要求补差的时间（期间不锁定接单方）
	TopupMarkedAt  int64 // 发布方登记补差订单号的时间
	ConfirmedAt    int64
	ConfirmMethod  string
	PaidAmountE8   int64
	Late           bool
	VoidReason     string
	DefaultedAt    int64
	SelfDeal       int64 // 疑似自导自演（不计信用）
	Unreadable     int64 // 复检连续读不到次数
	CreatedAt      int64
	UpdatedAt      int64

	Task   *Task
	Worker *User
}

func (s *Submission) Path() string { return "/s/" + s.Code }

// ---- 付款单 ----

type Payment struct {
	ID              int64
	SubmissionID    int64
	Kind            string // gateway | manual | topup | cert
	UserID          int64  // 付款方
	BPGOrderID      string
	MerchantOrderID string
	PayAmount       string
	BaseE8          int64
	NoteCode        string
	PayURL          string
	ReceiveUID      string
	ReceiveLink     string
	Status          string // pending | paid | underpaid | expired | closed
	ActualE8        int64
	MatchedBy       string
	BinanceOrderID  string
	PayerID         string
	PaidAt          int64
	ExpiresAt       int64
	RawCallback     string
	LastSyncAt      int64
	CreatedAt       int64
	UpdatedAt       int64
}

// ---- 申诉 ----

var disputeTypeText = map[string]string{
	"A": "未付款", "B": "标记已付但未收到", "C": "验证误判", "D": "推文不合格", "E": "任务违规", "F": "黑名单申诉",
}

func disputeType(t string) string {
	if v, ok := disputeTypeText[t]; ok {
		return v
	}
	return t
}

var disputeStatusText = map[string]string{
	"open": "已发起", "evidence": "举证中", "review": "管理员审理中", "jury": "小法庭投票中", "appeal": "上诉期", "resolved": "已结案",
}

func disputeStatus(s string) string {
	if v, ok := disputeStatusText[s]; ok {
		return v
	}
	return s
}

type Dispute struct {
	ID             int64
	Code           string
	Type           string
	SubmissionID   int64
	TaskID         int64
	OpenerID       int64
	AgainstID      int64
	Status         string
	EvidenceUntil  int64
	Resolution     string // upheld | rejected | partial
	ResolutionNote string
	ResolvedBy     int64 // 0 = 小法庭
	ResolvedAt     int64
	JuryCaseID     int64
	AppealUntil    int64
	AppealBy       int64
	CreatedAt      int64
	UpdatedAt      int64

	Opener  *User
	Against *User
	Sub     *Submission
	Task    *Task
}

func (d *Dispute) Path() string { return "/d/" + d.Code }

type DisputeMessage struct {
	ID        int64
	DisputeID int64
	AuthorID  int64 // 0 = 系统 / 管理员
	Text      string
	Images    []string
	CreatedAt int64
	Author    *User
}

// ---- 黑名单 ----

type BlacklistEntry struct {
	ID           int64
	UserID       int64
	XID          string
	Handle       string
	BinanceUID   string
	PayerID      string
	Role         string // publisher | worker
	Reason       string
	DisputeID    int64
	DisputeCode  string
	AmountOwedE8 int64
	RepaidAt     int64
	LiftedAt     int64
	LiftedBy     int64
	Note         string
	CreatedAt    int64
}

// ---- 小法庭 ----

type JuryCase struct {
	ID           int64
	DisputeID    int64
	Status       string // voting | closed | escalated
	Round        int64
	DeadlineAt   int64
	Verdict      string // for | against | none
	VotesFor     int64
	VotesAgainst int64
	Abstain      int64
	CreatedAt    int64
	ClosedAt     int64
}

type JuryVote struct {
	CaseID    int64
	JurorID   int64
	Vote      string
	Reason    string
	CreatedAt int64
}

// ---- 通知 ----

type Notification struct {
	ID        int64
	UserID    int64
	Kind      string
	Title     string
	Body      string
	Link      string
	ReadAt    int64
	CreatedAt int64
}

// ---- 信用等级 ----

type Tier struct {
	Name       string
	ExposureE8 int64
	OpenTasks  int64
	Concurrent int64
	Daily      int64
}

// PubStats 发布方公开统计。
type PubStats struct {
	Paid        int64
	PaidGateway int64 // 网关核销且非自导自演，升级只数这个
	Overdue     int64 // 逾期付款（含已补付）
	Defaulted   int64
	AvgPayMs    int64
	OpenTasks   int64
	ExposureE8  int64
	Frozen      bool
}

// WorkerStats 接单方公开统计。
type WorkerStats struct {
	Done        int64
	DoneGateway int64
	Void30d     int64
	Active      int64 // 进行中的接单
	Today       int64
	EarnedE8    int64
	Locked      bool
	Suspended   bool
	AwaitCount  int64
}

func joinLower(xs []string) string { return strings.ToLower(strings.Join(xs, "\n")) }
