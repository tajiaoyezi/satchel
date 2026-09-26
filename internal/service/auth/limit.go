package auth

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/base/ipaddr"
	coresecurity "github.com/satchel/satchel/internal/core/security"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// LoginLimits 是登录限流的参数，来自系统设置门这一组（login_rate_* 与 skip_local_ip，master-login-protection「登录限流」）。
type LoginLimits struct {
	MaxAttempts int
	Window      time.Duration
	Lock        time.Duration
	SkipLocalIP bool
}

// DefaultLoginLimits 是设置里没有值时的参数（照 mmwx：一小时内 5 次、锁一小时、跳过本地与内网地址的 IP 维度）。
func DefaultLoginLimits() LoginLimits {
	return LoginLimits{MaxAttempts: 5, Window: time.Hour, Lock: time.Hour, SkipLocalIP: true}
}

// 登录与第二步在安全事件里记的路径（与 REST 的会话入口一致）。
const (
	loginPath     = "/api/v1/session"
	twoFactorPath = "/api/v1/session/two-factor"
)

type attempts struct {
	count     int
	first     time.Time
	lockUntil time.Time
}

// limiter 是登录限流的两个内存表：按来源 IP 与按账号名数失败次数（照 mmwx 的 rate_limiter.go），重启即清空。
type limiter struct {
	mu     sync.Mutex
	limits LoginLimits
	ips    map[string]*attempts
	users  map[string]*attempts
}

func newLimiter() *limiter {
	return &limiter{limits: DefaultLoginLimits(), ips: map[string]*attempts{}, users: map[string]*attempts{}}
}

// ipKey 是 IP 维度的键；为空表示这次不计 IP 维度（没有来源 IP，或 skip_local_ip 下的本地与内网地址）。
func (l *limiter) ipKey(ip string) string {
	if ip == "" || (l.limits.SkipLocalIP && ipaddr.IsLocalOrPrivate(ip)) {
		return ""
	}
	return ip
}

// checkOne 照 mmwx 的 checkAttempts：锁定期内是锁；锁定期满或窗口过了就清掉；次数到了上限就从现在锁起。
func (l *limiter) checkOne(table map[string]*attempts, key string, now time.Time) (time.Time, bool) {
	a := table[key]
	if key == "" || a == nil {
		return time.Time{}, false
	}
	if !a.lockUntil.IsZero() {
		if now.Before(a.lockUntil) {
			return a.lockUntil, true
		}
		delete(table, key)
		return time.Time{}, false
	}
	if now.Sub(a.first) > l.limits.Window {
		delete(table, key)
		return time.Time{}, false
	}
	if a.count >= l.limits.MaxAttempts {
		a.lockUntil = now.Add(l.limits.Lock)
		return a.lockUntil, true
	}
	return time.Time{}, false
}

// reserve 在比对之前查两个维度，没锁就在同一把锁里给两个维度各先记上这一次（锁住时返回锁到什么时候，取两者较晚的）。
// 先记后比对，同一波并发的请求最多只有上限那么多个能走到比对：否则它们都会在计数还低时过关、各比对一次。
// 比对不上时这一次就留着（fail），整个成功就清零（succeed），不算一次猜测时退回去（release）。
func (l *limiter) reserve(ip, user string, now time.Time) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := l.ipKey(ip)
	if until, locked := l.checkLocked(key, user, now); locked {
		return until, true
	}
	l.recordOne(l.ips, key, now)
	l.recordOne(l.users, user, now)
	return time.Time{}, false
}

// release 退回 reserve 记上的这一次（验证码没过、密码对了但还要第二步、只缺第二因素、查库出错这类不算猜测的结局）。
func (l *limiter) release(ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range []struct {
		table map[string]*attempts
		key   string
	}{{l.ips, l.ipKey(ip)}, {l.users, user}} {
		if a := e.table[e.key]; e.key != "" && a != nil && a.count > 0 && a.lockUntil.IsZero() {
			a.count--
		}
	}
}

func (l *limiter) checkLocked(ipKey, user string, now time.Time) (time.Time, bool) {
	u1, byIP := l.checkOne(l.ips, ipKey, now)
	u2, byUser := l.checkOne(l.users, user, now)
	if u2.After(u1) {
		u1 = u2
	}
	return u1, byIP || byUser
}

// recordOne 照 mmwx 的 recordAttempt：第一次或窗口过了从 1 数起，否则加一。
func (l *limiter) recordOne(table map[string]*attempts, key string, now time.Time) int {
	if key == "" {
		return 0
	}
	a := table[key]
	if a == nil || now.Sub(a.first) > l.limits.Window {
		a = &attempts{first: now}
		table[key] = a
	}
	a.count++
	return a.count
}

// fail 确认 reserve 记上的这一次是失败（计数在 reserve 里已经加过），返回较大的那个计数、上限，以及某个维度是否因此锁上了。
func (l *limiter) fail(ip, user string, now time.Time) (count, max int, locked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := l.ipKey(ip)
	for _, a := range []*attempts{l.ips[key], l.users[user]} {
		if a != nil && a.count > count {
			count = a.count
		}
	}
	_, locked = l.checkLocked(key, user, now)
	return count, l.limits.MaxAttempts, locked
}

// sweep 清掉锁定期已满的、以及没锁而窗口已过的条目。
func (l *limiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, table := range []map[string]*attempts{l.ips, l.users} {
		for key, a := range table {
			if (a.lockUntil.IsZero() && now.Sub(a.first) > l.limits.Window) || (!a.lockUntil.IsZero() && !now.Before(a.lockUntil)) {
				delete(table, key)
			}
		}
	}
}

// Sweep 清一次登录限流的内存表（login_limit_sweep 任务每 10 分钟调一次）：条目本来只在同一个键再被访问时才清，
// 换着用户名猜的人会让表一直长。
func (s *Service) Sweep() {
	s.limiter.sweep(s.now())
}

// succeed 清零这个来源 IP 与这个账号的计数（整个登录成功、或当场验证通过时）。
func (l *limiter) succeed(ip, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if key := l.ipKey(ip); key != "" {
		delete(l.ips, key)
	}
	delete(l.users, user)
}

func rateLimited(until time.Time) *v1.Error {
	at := until.UTC().Format(time.RFC3339)
	return v1.New(v1.CodeRateLimited, "登录尝试次数过多，已暂时锁定").WithState("until", at).
		WithNext("等到 " + at + " 之后再试；主控本机经 unix socket 的操作不受这个限制")
}

// SetLoginLimits 换上新的登录限流参数（装配根在启动与每次设置写之后调），下一次判定就按它算。
func (s *Service) SetLoginLimits(l LoginLimits) {
	s.limiter.mu.Lock()
	defer s.limiter.mu.Unlock()
	s.limiter.limits = l
}

// SetEvents 接上安全事件的仓储：登录与当场验证比对不上时各记一条，写失败记到 logger（nil 时用 slog.Default()）。
// 没接时不记（单测与旧装配照旧）。
func (s *Service) SetEvents(events *coresecurity.Repo, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	s.events, s.logger = events, logger
}

// SetNow 换掉时钟（测试把时间往后拨，不用真的等）。
func (s *Service) SetNow(now func() time.Time) {
	s.now = now
}

// guarded 报告这个请求要不要受登录限流：经 unix socket 的不受（能连上 0600 socket 的只有 root 与运行主控的用户）。
func guarded(ctx context.Context) (v1.Remote, bool) {
	r := v1.RemoteFrom(ctx)
	return r, !r.Socket
}

// failed 记一次比对不上：reserve 占的这一次留着，写一条安全事件（触发锁定的那一次记 lockKind）。经 unix socket 的不计不记。
func (s *Service) failed(ctx context.Context, username, path, failKind, lockKind string) {
	r, ok := guarded(ctx)
	if !ok {
		return
	}
	count, max, locked := s.limiter.fail(r.IP, username, s.now())
	kind := failKind
	if locked {
		kind = lockKind
	}
	if s.events == nil {
		return
	}
	e := coresecurity.Event{At: s.now(), IP: r.IP, Kind: kind, Path: path, Username: username, Detail: fmt.Sprintf("%d/%d", count, max)}
	if err := s.events.InsertEvent(context.WithoutCancel(ctx), e); err != nil {
		s.logger.Error("写安全事件失败", "kind", e.Kind, "ip", e.IP, "path", e.Path, "username", e.Username, "detail", e.Detail, "error", err)
	}
}

// reserve 在比对之前查登录限流：锁定期内返回 rate_limited；没锁就先占上这一次（见 limiter.reserve），
// 之后必须以 failed、succeeded 或 released 之一收尾。经 unix socket 的不查不占。
func (s *Service) reserve(ctx context.Context, username string) error {
	r, ok := guarded(ctx)
	if !ok {
		return nil
	}
	if until, locked := s.limiter.reserve(r.IP, username, s.now()); locked {
		return rateLimited(until)
	}
	return nil
}

// released 退回 reserve 占的这一次：这次尝试没有比对出结果，不算一次猜测。
func (s *Service) released(ctx context.Context, username string) {
	if r, ok := guarded(ctx); ok {
		s.limiter.release(r.IP, username)
	}
}

// succeeded 清零计数（整个登录成功、或当场验证通过时）。
func (s *Service) succeeded(ctx context.Context, username string) {
	if r, ok := guarded(ctx); ok {
		s.limiter.succeed(r.IP, username)
	}
}
