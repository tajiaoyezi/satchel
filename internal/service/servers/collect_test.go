package servers

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/pkg/xrpc"
)

// snapConns 是只连着一台服务器、回给定快照的连接管理。
type snapConns struct {
	id   int64
	snap xrpc.TrafficSnapshot
}

func (c *snapConns) Call(context.Context, int64, string, any, time.Duration) (json.RawMessage, error) {
	return json.Marshal(c.snap)
}

func (c *snapConns) ConnectedIDs() []int64 { return []int64{c.id} }

// 写 core_boot_count 出问题时，重启次数既不漏记也不重复记（写的是绝对值）。三种情形各用一台新服务器：
//   - 写库失败：撤掉故障后，同一份快照把那次重启补上，之后不再加；
//   - 写进去了却没收到应答（用「失败之后手工把库里的值改成那次想写的值」模拟）：再拉一次仍是 1，不会变成 2；
//   - 库里本来就有次数：以它为起点往上加；第一次读起点失败时那份快照不丢；
//   - 一份快照跨两代：记 2 次；
//   - 内核代数 2、3 两次写库都失败，到 4 时写成功：次数是 3。
func TestCollectCoreBootCountUnderWriteFailures(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		settings := coresettings.New(bdb, st, schema.Default())
		if err := settings.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		repo := core.New(bdb, st)
		pg := db.DialectOf(bdb) == schema.Postgres
		if pg {
			if _, err := bdb.ExecContext(ctx, `CREATE FUNCTION fail_core() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'core 写不进去'; END $$`); err != nil {
				t.Fatal(err)
			}
		}
		// fail 让写 core_boot_count 的 UPDATE 失败（只在这一列出现在 SET 里时触发），返回撤掉故障的函数。
		fail := func() func() {
			t.Helper()
			create, drop := `CREATE TRIGGER fail_core BEFORE UPDATE OF core_boot_count ON servers BEGIN SELECT RAISE(ABORT, 'core 写不进去'); END`, `DROP TRIGGER fail_core`
			if pg {
				create, drop = `CREATE TRIGGER fail_core BEFORE UPDATE OF core_boot_count ON servers FOR EACH ROW EXECUTE FUNCTION fail_core()`, `DROP TRIGGER fail_core ON servers`
			}
			if _, err := bdb.ExecContext(ctx, create); err != nil {
				t.Fatal(err)
			}
			return func() {
				if _, err := bdb.ExecContext(ctx, drop); err != nil {
					t.Fatal(err)
				}
			}
		}
		start := time.Now()
		nodes := map[string]*Service{}
		// node 建一台服务器与只连着它的服务；collect(i, gen) 在第 i 个间隔收内核代数为 gen 的快照，返回 detail。
		node := func(name string) (collect func(i int, gen uint64) string, bootCount func() int64, id int64) {
			srv, err := repo.Create(ctx, name, "websocket")
			if err != nil {
				t.Fatal(err)
			}
			conns := &snapConns{id: srv.Metadata.ID}
			s := New(repo, settings, nil, nil)
			s.SetConns(conns)
			nodes[name] = s
			collect = func(i int, gen uint64) string {
				t.Helper()
				conns.snap = snap("b1", gen, count("vless", "direct", "a", 0, int64(gen)*100))
				detail, err := s.CollectTraffic(func() time.Time { return start.Add(time.Duration(i) * 2 * time.Minute) })(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return detail
			}
			bootCount = func() int64 {
				got, _ := repo.ByID(ctx, srv.Metadata.ID)
				return got.Status.CoreBootCount
			}
			return collect, bootCount, srv.Metadata.ID
		}

		// 写库失败，撤掉之后补上。
		collect, bootCount, _ := node("hk-1")
		collect(0, 1)
		undo := fail()
		if detail := collect(1, 2); detail != "拉了 0 台、失败 1 台" || bootCount() != 0 {
			t.Fatalf("写库失败的那次应当算失败、次数还没写进去：%s %d", detail, bootCount())
		}
		undo()
		if detail := collect(2, 2); detail != "拉了 1 台、失败 0 台" || bootCount() != 1 {
			t.Fatalf("故障撤掉后同一份快照应当把漏写的那次重启补上：%s %d", detail, bootCount())
		}
		if collect(3, 2); bootCount() != 1 {
			t.Fatalf("补上之后不应当再加：%d", bootCount())
		}

		// 写进去了却没收到应答：再拉同一份快照仍是 1。
		collect, bootCount, id := node("hk-2")
		collect(0, 1)
		undo = fail()
		collect(1, 2)
		undo()
		if _, err := bdb.ExecContext(ctx, `UPDATE servers SET core_boot_count = 1 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		if collect(2, 2); bootCount() != 1 {
			t.Fatalf("那次其实已经写进去时，重拉不应当再加一次：%d", bootCount())
		}

		// 库里本来就有次数（主控重启以前记的）：以它为起点往上加。
		collect, bootCount, id = node("hk-4")
		if _, err := bdb.ExecContext(ctx, `UPDATE servers SET core_boot_count = 5 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		collect(0, 1)
		if collect(1, 2); bootCount() != 6 {
			t.Fatalf("库里原有 5 次时，再检测到一次重启应当是 6：%d", bootCount())
		}

		// M-10.1：第一次读起点失败时直接返回且未推进累计器；恢复后先收到代数 1 作为起点（仍为 5），下一代代数 2 才记 5 → 6。
		collect, bootCount, id = node("hk-5")
		if _, err := bdb.ExecContext(ctx, `UPDATE servers SET core_boot_count = 5 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		svc := nodes["hk-5"]
		svc.conns.(*snapConns).snap = snap("b1", 1, count("vless", "direct", "a", 0, 100))
		if err := svc.collectOne(canceled, id, func() time.Time { return start }); err == nil {
			t.Fatal("读起点失败时这次拉取应当报错")
		}
		svc.mu.Lock()
		leakedAcc := svc.accs[id]
		svc.mu.Unlock()
		if leakedAcc != nil {
			t.Fatalf("读起点失败时不应当初始化内存累计器：%+v", leakedAcc)
		}
		if collect(1, 1); bootCount() != 5 {
			t.Fatalf("恢复后首份快照作为基线应保持 5：%d", bootCount())
		}
		if collect(2, 2); bootCount() != 6 {
			t.Fatalf("代数从 1 切到 2 应当是 6：%d", bootCount())
		}

		// 一份快照跨两代（代数 1 直接到 3）：记 2 次。
		collect, bootCount, _ = node("hk-6")
		collect(0, 1)
		if collect(1, 3); bootCount() != 2 {
			t.Fatalf("代数从 1 跳到 3 应当记 2 次：%d", bootCount())
		}

		// 连着两次写库失败期间内核又重启了一次：写成功时是 3。
		collect, bootCount, _ = node("hk-3")
		collect(0, 1)
		undo = fail()
		collect(1, 2)
		collect(2, 3)
		undo()
		if collect(3, 4); bootCount() != 3 {
			t.Fatalf("代数 2、3 写失败、4 写成功时应当是 3：%d", bootCount())
		}
	})
}

type connListConns struct {
	list xrpc.ConnectionsList
}

func (c *connListConns) Call(context.Context, int64, string, any, time.Duration) (json.RawMessage, error) {
	return json.Marshal(c.list)
}

func (c *connListConns) ConnectedIDs() []int64 { return nil }

// L-02 & H-01 回归：uint64 高位为 1 的连接 ID 分页不丢不错乱；shellSingleQuote 正确转义含单引号的参数。
func TestConnectionsUint64CursorAndInstallQuote(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st := store.New(bdb, schema.Default())
	settings := coresettings.New(bdb, st, schema.Default())
	if err := settings.EnsureSingleton(ctx); err != nil {
		t.Fatal(err)
	}
	repo := core.New(bdb, st)
	srv, err := repo.Create(ctx, "node-a", "websocket")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bdb.ExecContext(ctx, `UPDATE servers SET status = 'connected' WHERE id = ?`, srv.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	ids := []uint64{1, math.MaxInt64, uint64(math.MaxInt64) + 1, ^uint64(0)}
	conns := &connListConns{}
	for _, cid := range ids {
		conns.list.Connections = append(conns.list.Connections, xrpc.Connection{ID: cid, Network: "tcp", Peer: "1.2.3.4:1"})
	}
	s := New(repo, settings, nil, nil)
	s.SetConns(conns)

	// 按 limit=2 翻两页，确认每页 2 条且无符号顺序不被 int64 溢出打乱。
	res1Any, err := s.connections(ctx, &command.Invocation{Args: []string{"node-a"}, Flags: map[string]any{"inbound": "vless", "credential": "u"}, Page: &command.Page{Limit: 2}})
	if err != nil {
		t.Fatal(err)
	}
	res1 := res1Any.(*command.PageResult)
	if len(res1.Items) != 2 || res1.NextCursor == "" {
		t.Fatalf("第一页应当有 2 条与 nextCursor：%+v", res1)
	}
	if got0, got1 := res1.Items[0].(xrpc.Connection).ID, res1.Items[1].(xrpc.Connection).ID; got0 != 1 || got1 != math.MaxInt64 {
		t.Fatalf("第一页 ID 不对：%d %d", got0, got1)
	}
	res2Any, err := s.connections(ctx, &command.Invocation{Args: []string{"node-a"}, Flags: map[string]any{"inbound": "vless", "credential": "u"}, Page: &command.Page{Limit: 2, Cursor: res1.NextCursor}})
	if err != nil {
		t.Fatal(err)
	}
	res2 := res2Any.(*command.PageResult)
	if len(res2.Items) != 2 || res2.NextCursor != "" {
		t.Fatalf("第二页应当正好剩 2 条且无 nextCursor：%+v", res2)
	}
	if got0, got1 := res2.Items[0].(xrpc.Connection).ID, res2.Items[1].(xrpc.Connection).ID; got0 != uint64(math.MaxInt64)+1 || got1 != ^uint64(0) {
		t.Fatalf("第二页高位 uint64 ID 不对：%d %d", got0, got1)
	}

	if got := shellSingleQuote("a'b"); got != `'a'\''b'` || !strings.HasPrefix(got, "'") {
		t.Fatalf("shellSingleQuote 转义不对：%s", got)
	}
	if got := shellModeArg("weird mode'"); got != `'weird mode'\'''` {
		t.Fatalf("非安全字符集的 mode 也应当按单引号转义：%s", got)
	}
}
