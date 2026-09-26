package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/service/schedule"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// fakeRecorder 记下每次结束的任务名与错误。
type fakeRecorder struct {
	mu   sync.Mutex
	ends []ended
}

type ended struct {
	detail string
	err    error
}

func (f *fakeRecorder) Begin(context.Context, schedule.TaskInfo) *schedule.Run {
	return &schedule.Run{}
}
func (f *fakeRecorder) End(_ context.Context, _ *schedule.Run, detail string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, ended{detail: detail, err: err})
}

func (f *fakeRecorder) snapshot() []ended {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ended(nil), f.ends...)
}

func fast(tasks []Task, rec Recorder, first time.Duration) *Scheduler {
	s := New(tasks, rec, nil)
	s.firstDelay = first
	return s
}

func every(name string, d time.Duration, run func(context.Context) (string, error)) Task {
	return Task{Info: schedule.TaskInfo{Name: name, Every: d}, Run: run}
}

// master-scheduler「不重叠」：每次跑得比间隔久，任何时刻最多一次在跑。
func TestNoOverlap(t *testing.T) {
	var running, maxRunning, runs int32
	task := every("slow", time.Millisecond, func(context.Context) (string, error) {
		n := atomic.AddInt32(&running, 1)
		for {
			m := atomic.LoadInt32(&maxRunning)
			if n <= m || atomic.CompareAndSwapInt32(&maxRunning, m, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		atomic.AddInt32(&runs, 1)
		return "", nil
	})
	s := fast([]Task{task}, &fakeRecorder{}, time.Millisecond)
	s.Start(context.Background())
	time.Sleep(150 * time.Millisecond)
	s.Stop(context.Background())
	if maxRunning != 1 || runs < 3 {
		t.Fatalf("应当最多一次在跑且跑了几次：max %d，runs %d", maxRunning, runs)
	}
}

// 首次延迟：延迟没到之前不跑；任务以系统身份运行。
func TestFirstDelayAndIdentity(t *testing.T) {
	var got atomic.Value
	task := every("once", time.Hour, func(ctx context.Context) (string, error) {
		id := v1.IdentityFrom(ctx)
		got.Store(id)
		return "done", nil
	})
	rec := &fakeRecorder{}
	s := fast([]Task{task}, rec, 80*time.Millisecond)
	s.Start(context.Background())
	time.Sleep(30 * time.Millisecond)
	if len(rec.snapshot()) != 0 {
		t.Fatal("首次延迟没到就跑了")
	}
	time.Sleep(120 * time.Millisecond)
	s.Stop(context.Background())
	id, _ := got.Load().(v1.Identity)
	if len(rec.snapshot()) != 1 || id.Actor != "scheduler" || id.ActorKind != v1.ActorSystem || id.Role != v1.RoleAdmin {
		t.Fatalf("应当以系统身份跑一次：%+v %+v", rec.snapshot(), id)
	}
}

// panic 记成 error（panic: 开头），别的任务照常。
func TestPanicIsolated(t *testing.T) {
	var ok int32
	tasks := []Task{
		every("boom", 5*time.Millisecond, func(context.Context) (string, error) { panic("炸了") }),
		every("fine", 5*time.Millisecond, func(context.Context) (string, error) { atomic.AddInt32(&ok, 1); return "", nil }),
	}
	rec := &fakeRecorder{}
	s := fast(tasks, rec, time.Millisecond)
	s.Start(context.Background())
	time.Sleep(60 * time.Millisecond)
	s.Stop(context.Background())
	panics := 0
	for _, e := range rec.snapshot() {
		if e.err != nil {
			if !strings.HasPrefix(e.err.Error(), "panic: 炸了") {
				t.Fatalf("panic 的错误应当以 panic: 开头：%v", e.err)
			}
			panics++
		}
	}
	if panics < 2 || atomic.LoadInt32(&ok) < 2 {
		t.Fatalf("panic 的任务应当继续被调度、别的任务照常：panics %d，ok %d", panics, ok)
	}
}

// master-scheduler「停止时等任务结束」：Stop 等正在跑的任务返回，它的结束被记下。
func TestStopWaits(t *testing.T) {
	started := make(chan struct{})
	var finished atomic.Bool
	task := every("long", time.Hour, func(ctx context.Context) (string, error) {
		close(started)
		time.Sleep(80 * time.Millisecond) // 不理会 ctx，模拟一次收不住的运行
		finished.Store(true)
		return "", ctx.Err()
	})
	rec := &fakeRecorder{}
	s := fast([]Task{task}, rec, time.Millisecond)
	s.Start(context.Background())
	<-started
	s.Stop(context.Background())
	if !finished.Load() || len(rec.snapshot()) != 1 || !errors.Is(rec.snapshot()[0].err, context.Canceled) ||
		!strings.Contains(rec.snapshot()[0].err.Error(), "主控停止") {
		t.Fatalf("Stop 应当等任务返回并记下结束：finished %v，%+v", finished.Load(), rec.snapshot())
	}
	// Stop 的时限到了就不再等。
	block := make(chan struct{})
	defer close(block)
	started2 := make(chan struct{})
	s2 := fast([]Task{every("stuck", time.Hour, func(context.Context) (string, error) { close(started2); <-block; return "", nil })}, rec, time.Millisecond)
	s2.Start(context.Background())
	<-started2
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	begin := time.Now()
	s2.Stop(ctx)
	if time.Since(begin) > time.Second {
		t.Fatal("Stop 超过时限后应当返回")
	}
}
