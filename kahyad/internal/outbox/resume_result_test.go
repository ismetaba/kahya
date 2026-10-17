package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// FIX-FINAL tux-resumed-task-answer-never-delivered: a task the outbox
// redispatches (a cloud park, a crash, a kahyad restart mid-run) ran its
// worker with no OnDelta callback, so the answer the user was told would
// come ("otomatik devam edecek") was dropped: the task became 'done' and
// the text appeared on no surface. The answer now goes to the
// background-result channel (HANDOFF §4 IPC: Hammerspoon + Telegram) once,
// and only after the task really reached 'done'.

type deliveredNotice struct {
	traceID, text, spoken string
}

// recordingResultDelivery is a ResultDelivery that records every call.
type recordingResultDelivery struct {
	mu    sync.Mutex
	calls []deliveredNotice
	ok    bool
}

func (r *recordingResultDelivery) SendNotice(_ context.Context, traceID, text, spoken string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, deliveredNotice{traceID: traceID, text: text, spoken: spoken})
	return r.ok
}

func (r *recordingResultDelivery) snapshot() []deliveredNotice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deliveredNotice(nil), r.calls...)
}

// answerDispatcher redispatches through testdata/answer_worker.py <mode>
// with rec as the result delivery (nil = unwired).
func answerDispatcher(st *store.Store, m *task.Machine, mode string, rec ResultDelivery) *Dispatcher {
	d := newTestDispatcher(st, m, []string{"python3", "testdata/answer_worker.py", mode})
	if rec != nil {
		d.SetResultDelivery(rec)
	}
	return d
}

// dispatchOnce runs one claim pass and waits for every delivery it started.
func dispatchOnce(t *testing.T, d *Dispatcher) {
	t.Helper()
	claimed, err := d.ClaimAndDispatch(context.Background())
	if err != nil || claimed != 1 {
		t.Fatalf("ClaimAndDispatch = (%d, %v), want (1, nil)", claimed, err)
	}
	d.deliveries.Wait()
}

// resultDeliveredPayloads returns the payloads of every
// outbox.resume_result_delivered row.
func resultDeliveredPayloads(t *testing.T, st *store.Store) []map[string]any {
	t.Helper()
	rows, err := st.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY id`, EventResumeResultDelivered)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(raw, "Merhaba") || strings.Contains(raw, "dünya") {
			t.Errorf("the ledger row carries the answer text: %s", raw)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("decode payload %s: %v", raw, err)
		}
		out = append(out, p)
	}
	return out
}

func TestResumedTaskAnswerIsDeliveredOnceOnDone(t *testing.T) {
	for _, delivered := range []bool{true, false} {
		t.Run(fmt.Sprintf("delivered=%v", delivered), func(t *testing.T) {
			st := testStore(t)
			insertTaskWithEnvelope(t, st, "t1", "sess-original-123")
			enqueueResumeRow(t, st, "t1")
			m := task.NewMachine(st.Queries, st)
			rec := &recordingResultDelivery{ok: delivered}
			dispatchOnce(t, answerDispatcher(st, m, "ok", rec))

			if got := got0(t, st, "t1").Status; got != task.StatusDone {
				t.Fatalf("status = %q, want done", got)
			}
			calls := rec.snapshot()
			if len(calls) != 1 {
				t.Fatalf("deliveries = %d (%+v), want exactly 1", len(calls), calls)
			}
			want := deliveredNotice{
				traceID: "trace-t1",
				text:    "Devam ettirilen görev tamamlandı (t1). Cevap:\n\nMerhaba, dünya.",
				spoken:  "Devam ettirilen bir görev tamamlandı; cevabı bildirimde.",
			}
			if calls[0] != want {
				t.Errorf("delivery = %+v, want %+v", calls[0], want)
			}
			if fmt.Sprintf(MsgResumedAnswerFmt, "t1", "Merhaba, dünya.") != want.text || MsgResumedAnswerSpoken != want.spoken {
				t.Error("the pinned texts drifted from the constants")
			}
			rows := resultDeliveredPayloads(t, st)
			if len(rows) != 1 {
				t.Fatalf("%s rows = %d, want 1", EventResumeResultDelivered, len(rows))
			}
			if rows[0]["task_id"] != "t1" || rows[0]["delivered"] != delivered || rows[0]["bytes"] != float64(len("Merhaba, dünya.")) {
				t.Errorf("ledger payload = %v", rows[0])
			}
		})
	}
}

func TestResumedTaskAnswerNotDeliveredWithoutACleanFinish(t *testing.T) {
	for _, mode := range []string{"crash", "error", "hang"} {
		t.Run(mode, func(t *testing.T) {
			st := testStore(t)
			insertTaskWithEnvelope(t, st, "t1", "sess-original-123")
			enqueueResumeRow(t, st, "t1")
			m := task.NewMachine(st.Queries, st)
			rec := &recordingResultDelivery{ok: true}
			d := answerDispatcher(st, m, mode, rec)
			if mode == "hang" {
				d.SetTaskTimeout(500 * time.Millisecond)
			}
			dispatchOnce(t, d)

			if got := got0(t, st, "t1").Status; got == task.StatusDone {
				t.Fatalf("status = done after a %s run", mode)
			}
			if calls := rec.snapshot(); len(calls) != 0 {
				t.Errorf("a %s run delivered %d answers: %+v", mode, len(calls), calls)
			}
			if rows := resultDeliveredPayloads(t, st); len(rows) != 0 {
				t.Errorf("a %s run ledgered %v", mode, rows)
			}
		})
	}
}

// failDoneStore refuses the transition to 'done' (a halt or a concurrent
// terminal transition that won the compare-and-set).
type failDoneStore struct{ *sqlcgen.Queries }

func (f failDoneStore) SetTaskStatus(ctx context.Context, arg sqlcgen.SetTaskStatusParams) (int64, error) {
	if arg.Status == task.StatusDone {
		return 0, errors.New("forced: done transition refused")
	}
	return f.Queries.SetTaskStatus(ctx, arg)
}

func TestResumedTaskAnswerNotDeliveredWhenDoneTransitionFails(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-original-123")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(failDoneStore{st.Queries}, st)
	rec := &recordingResultDelivery{ok: true}
	dispatchOnce(t, answerDispatcher(st, m, "ok", rec))

	if got := got0(t, st, "t1").Status; got == task.StatusDone {
		t.Fatal("status = done although the transition was refused")
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("an answer was delivered for a task that never reached done: %+v", calls)
	}
	if n := countEvents(t, st, "outbox.transition_failed"); n != 1 {
		t.Errorf("outbox.transition_failed rows = %d, want 1", n)
	}
}

func TestResumedTaskEmptyAnswerIsLedgeredNotDelivered(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-original-123")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	rec := &recordingResultDelivery{ok: true}
	dispatchOnce(t, answerDispatcher(st, m, "empty", rec))

	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("an empty answer was delivered: %+v", calls)
	}
	rows := resultDeliveredPayloads(t, st)
	if len(rows) != 1 || rows[0]["delivered"] != false || rows[0]["reason"] != "empty" {
		t.Errorf("ledger rows = %v, want one {delivered:false, reason:empty}", rows)
	}
}

func TestResumedTaskLongAnswerIsBounded(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-original-123")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	rec := &recordingResultDelivery{ok: true}
	dispatchOnce(t, answerDispatcher(st, m, "long", rec))

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(calls))
	}
	want := fmt.Sprintf(MsgResumedAnswerFmt, "t1", strings.Repeat("ş", maxResumedAnswerRunes)) + MsgResumedAnswerTruncated
	if calls[0].text != want {
		t.Errorf("long answer text = %d runes, want %d (bounded + truncation note)", utf8.RuneCountInString(calls[0].text), utf8.RuneCountInString(want))
	}
	if MsgResumedAnswerTruncated != "\n\n… (cevabın devamı kısaltıldı)" {
		t.Errorf("MsgResumedAnswerTruncated = %q", MsgResumedAnswerTruncated)
	}
	if rows := resultDeliveredPayloads(t, st); len(rows) != 1 || rows[0]["truncated"] != true {
		t.Errorf("ledger rows = %v, want one with truncated:true", rows)
	}
}

func TestResumedTaskWithoutResultDeliveryIsUnchanged(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-original-123")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	dispatchOnce(t, answerDispatcher(st, m, "ok", nil))
	if got := got0(t, st, "t1").Status; got != task.StatusDone {
		t.Fatalf("status = %q, want done", got)
	}
	if rows := resultDeliveredPayloads(t, st); len(rows) != 0 {
		t.Errorf("an unwired delivery ledgered %v", rows)
	}
}

var _ ResultDelivery = (*recordingResultDelivery)(nil)
