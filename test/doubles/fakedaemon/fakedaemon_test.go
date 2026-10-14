package fakedaemon

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/spawn"
	"go.uber.org/goleak"
)

// This module cannot import the task package the rest of the repository
// starts goroutines on, so goleak holds its tests directly: a goroutine left
// running once they are done fails them.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// The fake's SHAPES are held to the contract in the daemon's repository, with
// the harness that judges the real daemon. What is here is its behaviour, driven through the generated client —
// the same one the SDK is built on — so it is tested from this module and by
// its own tests.

func typed(t *testing.T, d *Daemon, key string) *genv1.ClientWithResponses {
	t.Helper()
	opts := []genv1.ClientOption{}
	if key != "" {
		opts = append(opts, genv1.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+key)
			return nil
		}))
	}
	client, err := genv1.NewClientWithResponses(d.URL, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// A sandbox's whole life, through every operation the fake answers.
func TestTheFakeDoesWhatTheDaemonDoes(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("k"), WithBrokers("claude"), WithRun(func(_ context.Context, _ genv1.SandboxID, request genv1.CommandRequest) genv1.CommandResult {
		return genv1.CommandResult{ExitCode: 3, Stdout: strings.Join(request.Argv, " ")}
	}), WithBoot(func(genv1.SandboxSpec) Outcome { return Outcome{Console: "Linux version 6.12\n"} }))
	client := typed(t, d, "k")

	info, err := client.GetInfoWithResponse(ctx)
	if err != nil || info.JSON200 == nil {
		t.Fatalf("info = %v %s", err, info.Body)
	}
	shared, err := client.ListBrokersWithResponse(ctx, DefaultNamespace)
	if err != nil || shared.JSON200 == nil || len(shared.JSON200.Items) != 1 || shared.JSON200.Items[0].Name != "claude" {
		t.Fatalf("brokers = %v %s", err, shared.Body)
	}
	volume := "chat-1"
	labels := map[string]string{"name": "one"}
	spec := genv1.SandboxSpec{
		Image:   "alpine:3.20",
		Labels:  &labels,
		Brokers: &[]genv1.SandboxBrokerSpec{{Name: "claude"}},
		Disk:    &genv1.DiskSpec{Volume: &volume},
	}
	created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "key-1"}, spec)
	if err != nil || created.JSON202 == nil || created.JSON202.State != genv1.SandboxStateCreating {
		t.Fatalf("create = %v %d %s", err, created.StatusCode(), created.Body)
	}
	id := created.JSON202.Id
	if again, _ := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "key-1"}, spec); again.JSON200 == nil || again.JSON200.Id != id {
		t.Fatalf("a repeat with the same key = %d %s, want 200 and the same sandbox", again.StatusCode(), again.Body)
	}
	if other, _ := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "key-1"}, genv1.SandboxSpec{Image: "alpine:3.21"}); other.JSON409 == nil || other.JSON409.Error.Code != genv1.ErrorCodeSpecConflict {
		t.Fatalf("the same key with another spec = %d %s, want 409 spec_conflict", other.StatusCode(), other.Body)
	}
	if held, _ := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "key-2"}, genv1.SandboxSpec{Image: "alpine:3.20", Disk: &genv1.DiskSpec{Volume: &volume}}); held.JSON409 == nil || held.JSON409.Error.Code != genv1.ErrorCodeVolumeInUse {
		t.Fatalf("a create naming a held volume = %d %s", held.StatusCode(), held.Body)
	}
	if got, _ := client.GetSandboxWithResponse(ctx, DefaultNamespace, id); got.JSON200 == nil || got.JSON200.State != genv1.SandboxStateReady || (*got.JSON200.Brokers)[0].Port == 0 {
		t.Fatalf("the sandbox is %s", got.Body)
	}
	if got, _ := client.ListSandboxesWithResponse(ctx, DefaultNamespace, &genv1.ListSandboxesParams{}); got.JSON200 == nil || len(got.JSON200.Items) != 1 || got.JSON200.Items[0].Id != id {
		t.Fatalf("the list does not have it: %s", got.Body)
	}
	if events, _ := client.StreamSandboxEventsWithResponse(ctx, DefaultNamespace, id, &genv1.StreamSandboxEventsParams{}); !strings.Contains(string(events.Body), `"state":"creating"`) || !strings.Contains(string(events.Body), `"state":"ready"`) {
		t.Fatalf("events: %s", events.Body)
	}
	if got, _ := client.GetSandboxConsoleWithResponse(ctx, DefaultNamespace, id, &genv1.GetSandboxConsoleParams{}); string(got.Body) != "Linux version 6.12\n" {
		t.Fatalf("console: %q", got.Body)
	}
	if ran, _ := client.RunCommandWithResponse(ctx, DefaultNamespace, id, genv1.CommandRequest{Argv: []string{"echo", "hi"}}); ran.JSON200 == nil || ran.JSON200.ExitCode != 3 || ran.JSON200.Stdout != "echo hi" {
		t.Fatalf("run: %s", ran.Body)
	}
	if got := d.Commands(); len(got) != 1 || got[0].Argv[1] != "hi" {
		t.Fatalf("the commands the fake recorded: %+v", got)
	}
	mode := "0600"
	if got, _ := client.WriteFileWithBodyWithResponse(ctx, DefaultNamespace, id, &genv1.WriteFileParams{Path: "/work/a", Mode: &mode}, "application/octet-stream", bytes.NewReader([]byte("bytes"))); got.StatusCode() != 204 {
		t.Fatalf("write = %d %s", got.StatusCode(), got.Body)
	}
	if body, gotMode, ok := d.File(id, "/work/a"); !ok || string(body) != "bytes" || gotMode != "0600" {
		t.Fatalf("the fake holds %q mode %q (%v)", body, gotMode, ok)
	}
	if got, _ := client.ReadFileWithResponse(ctx, DefaultNamespace, id, &genv1.ReadFileParams{Path: "/work/a"}); string(got.Body) != "bytes" {
		t.Fatalf("read back %q", got.Body)
	}
	if got, _ := client.ReadFileWithResponse(ctx, DefaultNamespace, id, &genv1.ReadFileParams{Path: "/work/none"}); got.JSON404 == nil {
		t.Fatalf("a missing file = %d", got.StatusCode())
	}
	if got, _ := client.ListVolumesWithResponse(ctx, DefaultNamespace); got.JSON200 == nil || got.JSON200.Items[0].SandboxId == nil || *got.JSON200.Items[0].SandboxId != id {
		t.Fatalf("the volume does not say who holds it: %s", got.Body)
	}
	if got, _ := client.DeleteVolumeWithResponse(ctx, DefaultNamespace, volume); got.JSON409 == nil {
		t.Fatalf("deleting a held volume = %d", got.StatusCode())
	}
	keep := genv1.DeleteSandboxParamsDisk("keep")
	if got, _ := client.DeleteSandboxWithResponse(ctx, DefaultNamespace, id, &genv1.DeleteSandboxParams{Disk: &keep}); got.StatusCode() != 204 {
		t.Fatalf("delete = %d %s", got.StatusCode(), got.Body)
	}
	if got := d.Deletes(); len(got) != 1 || got[0].ID != id || got[0].Disk != "keep" {
		t.Fatalf("the deletes the fake recorded: %+v", got)
	}
	if got, _ := client.GetSandboxWithResponse(ctx, DefaultNamespace, id); got.StatusCode() != 404 {
		t.Fatalf("a dropped sandbox = %d", got.StatusCode())
	}
	if got, _ := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "key-1"}, spec); got.StatusCode() != 409 {
		t.Fatalf("a key whose sandbox was dropped = %d, want 409", got.StatusCode())
	}
	if got := d.IdempotencyKeys(); len(got) < 3 {
		t.Fatalf("the keys the fake recorded: %v", got)
	}
	if exists, holder := d.Volume(volume); !exists || holder != nil {
		t.Fatalf("a kept volume: exists %v, held by %v", exists, holder)
	}
	for range 2 {
		// The second is a volume that is not there, which is also a 204.
		if got, _ := client.DeleteVolumeWithResponse(ctx, DefaultNamespace, volume); got.StatusCode() != 204 {
			t.Fatalf("deleting a free volume = %d", got.StatusCode())
		}
	}
	if got, _ := typed(t, d, "wrong").GetInfoWithResponse(ctx); got.JSON401 == nil {
		t.Fatalf("a wrong key = %d", got.StatusCode())
	}
}

// A volume says who made it, and when it was last taken or let go: one a
// test puts there was made by nobody.
func TestTheFakeSaysWhoMadeAVolumeAndWhenItWasLastUsed(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	clock := now
	d := New(t, WithKey("k"), WithClock(func() time.Time { return clock }))
	client := typed(t, d, "k")
	ctx := context.Background()
	spec := genv1.SandboxSpec{Image: "alpine", Disk: &genv1.DiskSpec{Volume: new("chat")}}
	created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "k1"}, spec)
	if err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}
	d.PutVolume("other", 1)

	got, _ := client.ListVolumesWithResponse(ctx, DefaultNamespace)
	if got.JSON200 == nil || len(got.JSON200.Items) != 2 {
		t.Fatalf("listed = %s", got.Body)
	}
	chat := got.JSON200.Items[0]
	if chat.Name != "chat" || chat.CreatedBy == nil || *chat.CreatedBy.KeyId != "koperator" || !chat.LastUsedAt.Equal(now) {
		t.Errorf("chat = %+v", chat)
	}
	if other := got.JSON200.Items[1]; other.CreatedBy != nil || !other.LastUsedAt.Equal(now) {
		t.Errorf("other = %+v", other)
	}

	clock = now.Add(time.Minute)
	if got, _ := client.DeleteSandboxWithResponse(ctx, DefaultNamespace, created.JSON202.Id, nil); got.StatusCode() != 204 {
		t.Fatalf("delete = %d", got.StatusCode())
	}
	all, _ := client.ListVolumesWithResponse(ctx, DefaultNamespace)
	if len(all.JSON200.Items) != 2 || !all.JSON200.Items[0].LastUsedAt.Equal(clock) {
		t.Errorf("all = %+v", all.JSON200.Items)
	}
}

func TestPutVolumeIsAVolumeNobodyHolds(t *testing.T) {
	d := New(t)
	d.PutVolume("seeded", 4096)
	got, err := typed(t, d, "").ListVolumesWithResponse(context.Background(), DefaultNamespace)
	if err != nil || got.JSON200 == nil || len(got.JSON200.Items) != 1 {
		t.Fatalf("volumes = %v %s", err, got.Body)
	}
	if v := got.JSON200.Items[0]; v.Name != "seeded" || v.SandboxId != nil {
		t.Fatalf("the volume is %+v", v)
	}
}

// What is in one namespace is in no other: its sandboxes, its volumes — a
// name in two is two volumes — and its counts; and every body says which
// namespace it is in. What is about the host is about every namespace.
func TestEachNamespaceHoldsItsOwn(t *testing.T) {
	d := New(t, WithKey("k"), WithNamespaces("acme", "globex"), WithGlobalKeys())
	client := typed(t, d, "k")
	ctx := t.Context()
	spec := genv1.SandboxSpec{Image: "alpine", Disk: &genv1.DiskSpec{Volume: new("chat")}, Cpus: new(2)}
	ids := map[string]genv1.SandboxID{}
	for _, namespace := range []string{"acme", "globex"} {
		made, err := client.CreateSandboxWithResponse(ctx, namespace, &genv1.CreateSandboxParams{IdempotencyKey: "one"}, spec)
		if err != nil || made.JSON202 == nil {
			t.Fatalf("a create naming chat in %s = %v %d %s", namespace, err, made.StatusCode(), made.Body)
		}
		ids[namespace] = made.JSON202.Id
	}
	d.PutVolume("seeded", 1)
	for namespace, volumes := range map[string]string{"acme": "chat,seeded", "globex": "chat"} {
		list, _ := client.ListSandboxesWithResponse(ctx, namespace, nil)
		if list.JSON200 == nil || len(list.JSON200.Items) != 1 || list.JSON200.Items[0].Id != ids[namespace] || *list.JSON200.Items[0].Namespace != namespace {
			t.Errorf("%s's sandboxes = %s", namespace, list.Body)
		}
		held, _ := client.ListVolumesWithResponse(ctx, namespace)
		var names []string
		for _, v := range held.JSON200.Items {
			names = append(names, v.Name)
			if *v.Namespace != namespace {
				t.Errorf("%s's volume %s says it is in %s", namespace, v.Name, *v.Namespace)
			}
		}
		if strings.Join(names, ",") != volumes {
			t.Errorf("%s's volumes = %v, want %s", namespace, names, volumes)
		}
		counted, _ := client.GetCountsWithResponse(ctx, namespace)
		if counted.JSON200 == nil || *counted.JSON200.Sandboxes != 1 || *counted.JSON200.Volumes != len(held.JSON200.Items) {
			t.Errorf("%s's counts = %s", namespace, counted.Body)
		}
	}
	// globex's chat is globex's alone to delete, and acme's is still held.
	if got, _ := client.DeleteSandboxWithResponse(ctx, "globex", ids["globex"], &genv1.DeleteSandboxParams{Disk: new(genv1.DeleteSandboxParamsDisk("delete"))}); got.StatusCode() != http.StatusNoContent {
		t.Fatalf("delete = %d", got.StatusCode())
	}
	if got, _ := client.DeleteVolumeWithResponse(ctx, "acme", "chat"); got.StatusCode() != http.StatusConflict {
		t.Errorf("acme's held chat = %d, want 409", got.StatusCode())
	}
	if exists, holder := d.Volume("chat"); !exists || holder == nil || *holder != ids["acme"] {
		t.Errorf("acme's chat: exists %v, held by %v", exists, holder)
	}
	// acme's seeded, dropped from globex, is a name globex has no volume by.
	if got, _ := client.DeleteVolumeWithResponse(ctx, "globex", "seeded"); got.StatusCode() != http.StatusNoContent {
		t.Errorf("acme's seeded, dropped from globex = %d", got.StatusCode())
	}
	if exists, _ := d.Volume("seeded"); !exists {
		t.Error("acme's seeded went when globex dropped a volume by its name")
	}
	commitments, _ := client.GetHostCommitmentsWithResponse(ctx)
	if commitments.JSON200 == nil || commitments.JSON200.Running != 1 || commitments.JSON200.Cpus != 2 {
		t.Errorf("the host's commitments = %s", commitments.Body)
	}
}

// A followed stream is held open until the sandbox settles, and ends when the
// sandbox is dropped.
func TestAFollowedStreamEndsWhenTheSandboxIsDropped(t *testing.T) {
	d := New(t, WithBoot(func(genv1.SandboxSpec) Outcome { return Outcome{State: genv1.SandboxStateCreating} }))
	client := typed(t, d, "")
	created, err := client.CreateSandboxWithResponse(context.Background(), DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "k"}, genv1.SandboxSpec{Image: "alpine"})
	if err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v", err)
	}
	id := created.JSON202.Id
	<-d.Created()

	res, err := http.Get(d.URL + "/v1/namespaces/default/sandboxes/" + id.String() + "/events?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	d.Settle(id, genv1.SandboxStateFailed, "the kernel panicked")
	if got, _ := client.DeleteSandboxWithResponse(context.Background(), DefaultNamespace, id, &genv1.DeleteSandboxParams{}); got.StatusCode() != 204 {
		t.Fatalf("delete = %d", got.StatusCode())
	}
	done := make(chan string, 1)
	// Ended by the body's close, deferred above, if the stream never is.
	spawn.Go(t, func() {
		var body strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			body.Write(buf[:n])
			if err != nil {
				done <- body.String()
				return
			}
		}
	})
	select {
	case body := <-done:
		if !strings.Contains(body, `"state":"failed"`) || !strings.Contains(body, "the kernel panicked") {
			t.Fatalf("the stream said %s", body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream of a dropped sandbox stayed open")
	}
}

// The interceptors are what the SDK's failure paths are tested with, so they
// have to do what they say.
func TestInterceptorsAnswerInTheDaemonsPlace(t *testing.T) {
	d := New(t)
	d.Intercept("getInfo", Refuse(503, genv1.ErrorCodeInternal, "busy"))
	d.Intercept("getInfo", Answer(502, "text/html", "<h1>Bad Gateway</h1>"))
	d.Intercept("getInfo", HangUp())
	d.InterceptAll("listVolumes", Answer(500, "", "no"))

	// A fresh connection each time: net/http quietly retries an idempotent
	// request once when a REUSED connection is hung up on, which would make
	// the hang-up below answer with whatever came next.
	fresh := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(path string) (int, string, error) {
		res, err := fresh.Get(d.URL + path)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body), nil
	}
	if status, body, _ := get("/v1/info"); status != 503 || !strings.Contains(body, `"internal"`) {
		t.Errorf("first = %d %s", status, body)
	}
	if status, body, _ := get("/v1/info"); status != 502 || body != "<h1>Bad Gateway</h1>" {
		t.Errorf("second = %d %s", status, body)
	}
	if _, _, err := get("/v1/info"); err == nil {
		t.Error("a hang-up answered")
	}
	if status, _, _ := get("/v1/info"); status != 200 {
		t.Errorf("once the queue is spent the daemon answers: %d", status)
	}
	for range 2 {
		if status, _, _ := get("/v1/namespaces/default/volumes"); status != 500 {
			t.Errorf("InterceptAll answered %d", status)
		}
	}
	if got := d.Calls("getInfo"); got != 4 {
		t.Errorf("getInfo was counted %d times, want 4", got)
	}
}

// HangUpAfter is a create that happened and an answer that never arrived.
func TestHangUpAfterDoesTheThingAndLosesTheAnswer(t *testing.T) {
	d := New(t)
	d.Intercept("createSandbox", HangUpAfter())
	req, _ := http.NewRequest(http.MethodPost, d.URL+"/v1/namespaces/default/sandboxes", strings.NewReader(`{"image":"alpine"}`))
	req.Header.Set("Idempotency-Key", "k")
	if res, err := http.DefaultClient.Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatal("the create was answered")
	}
	if got := len(d.Sandboxes()); got != 1 {
		t.Fatalf("%d sandboxes exist, want the one the lost create made", got)
	}
}

// A reply the double cannot encode is a 500 that says so, not a success whose
// body stops halfway.
func TestAReplyThatCannotBeEncodedIsAnError(t *testing.T) {
	recorder := httptest.NewRecorder()
	reply(recorder, http.StatusOK, map[string]any{"unencodable": make(chan int)})
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "encoding the reply") {
		t.Fatalf("reply = %d %q", recorder.Code, recorder.Body.String())
	}
}

// Its egress plane: rules kept in the spec, put and deleted by name, replaced
// as a set, and refused where the daemon refuses them.
func TestTheFakeKeepsASandboxsEgressRules(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("k"), WithNetwork())
	client := typed(t, d, "k")
	if info, _ := client.GetInfoWithResponse(ctx); info.JSON200 == nil || !info.JSON200.Network.Enabled {
		t.Fatalf("a host with a network says %s", info.Body)
	}
	if info, _ := typed(t, New(t), "").GetInfoWithResponse(ctx); info.JSON200 == nil || info.JSON200.Network.Enabled {
		t.Fatalf("a host without one says %s", info.Body)
	}
	created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "e"}, genv1.SandboxSpec{Image: "alpine:3.20"})
	if err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}
	id := created.JSON202.Id
	if rules := d.Rules(id); rules != nil {
		t.Fatalf("a sandbox made with none has %+v", rules)
	}
	if got, _ := client.ListEgressRulesWithResponse(ctx, DefaultNamespace, id); got.JSON200 == nil || len(got.JSON200.Items) != 0 {
		t.Fatalf("no rules = %s", got.Body)
	}

	domains := []string{"ifconfig.me"}
	if put, _ := client.PutEgressRuleWithResponse(ctx, DefaultNamespace, id, "ifconfig", genv1.EgressRuleSpec{Domains: &domains}); put.JSON201 == nil || put.JSON201.Name != "ifconfig" {
		t.Fatalf("adding = %d %s", put.StatusCode(), put.Body)
	}
	ports := []string{"tcp/443"}
	if put, _ := client.PutEgressRuleWithResponse(ctx, DefaultNamespace, id, "ifconfig", genv1.EgressRuleSpec{Domains: &domains, Ports: &ports}); put.JSON200 == nil {
		t.Fatalf("replacing = %d %s", put.StatusCode(), put.Body)
	}
	cidrs := []string{"192.0.2.0/24"}
	if put, _ := client.PutEgressRuleWithResponse(ctx, DefaultNamespace, id, "mirror", genv1.EgressRuleSpec{Cidrs: &cidrs}); put.JSON201 == nil {
		t.Fatalf("adding another = %d %s", put.StatusCode(), put.Body)
	}
	if got, _ := client.GetEgressRuleWithResponse(ctx, DefaultNamespace, id, "ifconfig"); got.JSON200 == nil || (*got.JSON200.Ports)[0] != "tcp/443" {
		t.Fatalf("reading one = %s", got.Body)
	}
	if got, _ := client.GetEgressRuleWithResponse(ctx, DefaultNamespace, id, "nope"); got.StatusCode() != http.StatusNotFound {
		t.Fatalf("reading one it does not have = %d", got.StatusCode())
	}
	if got, _ := client.ListEgressRulesWithResponse(ctx, DefaultNamespace, id); got.JSON200 == nil || len(got.JSON200.Items) != 2 {
		t.Fatalf("both = %s", got.Body)
	}

	d.RecordRefusal(id, genv1.EgressRefusal{Target: "first.example", At: time.Now()})
	d.RecordRefusal(id, genv1.EgressRefusal{Target: "second.example", At: time.Now()})
	d.RecordRefusal(genv1.SandboxID{}, genv1.EgressRefusal{Target: "nobody"})
	report, _ := client.GetSandboxEgressWithResponse(ctx, DefaultNamespace, id)
	if report.JSON200 == nil || len(*report.JSON200.Policy.Rules) != 2 || len(report.JSON200.Refusals) != 2 || report.JSON200.Refusals[0].Target != "second.example" {
		t.Fatalf("the report = %s", report.Body)
	}

	if del, _ := client.DeleteEgressRuleWithResponse(ctx, DefaultNamespace, id, "ifconfig"); del.StatusCode() != http.StatusNoContent {
		t.Fatalf("deleting = %d %s", del.StatusCode(), del.Body)
	}
	if del, _ := client.DeleteEgressRuleWithResponse(ctx, DefaultNamespace, id, "ifconfig"); del.StatusCode() != http.StatusNotFound {
		t.Fatalf("deleting it again = %d", del.StatusCode())
	}
	if rules := d.Rules(id); len(rules) != 1 || rules[0].Name != "mirror" {
		t.Fatalf("after deleting: %+v", rules)
	}

	set := []genv1.EgressRule{{Name: "only", Domains: &domains}}
	if res, _ := client.SetSandboxEgressWithResponse(ctx, DefaultNamespace, id, genv1.EgressSpec{Rules: &set}); res.StatusCode() != http.StatusNoContent {
		t.Fatalf("replacing the set = %d %s", res.StatusCode(), res.Body)
	}
	if rules := d.Rules(id); len(rules) != 1 || rules[0].Name != "only" {
		t.Fatalf("after replacing: %+v", rules)
	}
	if res, _ := client.SetSandboxEgressWithResponse(ctx, DefaultNamespace, id, genv1.EgressSpec{}); res.StatusCode() != http.StatusNoContent || len(d.Rules(id)) != 0 {
		t.Fatalf("no rules = %d %+v", res.StatusCode(), d.Rules(id))
	}
}

func TestTheFakeRefusesWhatTheDaemonRefusesAboutRules(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithBoot(func(genv1.SandboxSpec) Outcome { return Outcome{State: genv1.SandboxStateCreating} }))
	client := typed(t, d, "")
	created, _ := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "e"}, genv1.SandboxSpec{Image: "alpine:3.20"})
	id := created.JSON202.Id
	domains := []string{"a.example"}
	raw := func(method, path, body string) int {
		t.Helper()
		req, err := http.NewRequest(method, d.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	base := "/v1/namespaces/default/sandboxes/" + id.String()

	// Still creating: its rules cannot change yet.
	if put, _ := client.PutEgressRuleWithResponse(ctx, DefaultNamespace, id, "a", genv1.EgressRuleSpec{Domains: &domains}); put.StatusCode() != http.StatusConflict {
		t.Fatalf("putting on a creating sandbox = %d", put.StatusCode())
	}
	if del, _ := client.DeleteEgressRuleWithResponse(ctx, DefaultNamespace, id, "a"); del.StatusCode() != http.StatusConflict {
		t.Fatalf("deleting on a creating sandbox = %d", del.StatusCode())
	}
	if res, _ := client.SetSandboxEgressWithResponse(ctx, DefaultNamespace, id, genv1.EgressSpec{}); res.StatusCode() != http.StatusConflict {
		t.Fatalf("setting on a creating sandbox = %d", res.StatusCode())
	}
	d.Settle(id, genv1.SandboxStateReady, "")

	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{"PUT", "/egress/rules/a", `{"domains": ["a.example"], "allow": ["b"]}`, http.StatusBadRequest},
		{"PUT", "/egress/rules/a", `{}`, http.StatusBadRequest},
		{"PUT", "/egress/rules/Not_A_Name", `{"domains": ["a.example"]}`, http.StatusBadRequest},
		{"GET", "/egress/rules/Not_A_Name", ``, http.StatusBadRequest},
		{"DELETE", "/egress/rules/Not_A_Name", ``, http.StatusBadRequest},
		{"PUT", "/egress", `{"allow": ["a.example"]}`, http.StatusBadRequest},
		{"PUT", "/egress", `{"rules": [{"name": "a"}]}`, http.StatusBadRequest},
		{"PUT", "/egress", `{"rules": [{"name": "a", "cidrs": ["10.0.0.0/8"]}, {"name": "a", "cidrs": ["10.0.0.0/8"]}]}`, http.StatusBadRequest},
		{"PUT", "/egress/rules/a", `{"internet": false}`, http.StatusBadRequest},
		// The internet is something to open, alone.
		{"PUT", "/egress/rules/internet", `{"internet": true, "ports": ["tcp/1-65535"]}`, http.StatusCreated},
		{"GET", "/egress/rules/internet", ``, http.StatusOK},
		{"PUT", "/egress", `{"rules": [{"name": "internet", "internet": true}]}`, http.StatusNoContent},
		{"GET", "/egress/rules", ``, http.StatusOK},
	} {
		if got := raw(c.method, base+c.path, c.body); got != c.status {
			t.Errorf("%s %s %s = %d, want %d", c.method, c.path, c.body, got, c.status)
		}
	}
	missing := "/v1/namespaces/default/sandboxes/0192f7a4-5b1e-7c3d-9a2f-4e6b8c1d0a53"
	for _, path := range []string{"/egress", "/egress/rules", "/egress/rules/a"} {
		if got := raw("GET", missing+path, ""); got != http.StatusNotFound {
			t.Errorf("GET %s of a missing sandbox = %d", path, got)
		}
	}
	if put, _ := client.PutEgressRuleWithResponse(ctx, DefaultNamespace, [16]byte{1}, "a", genv1.EgressRuleSpec{Domains: &domains}); put.StatusCode() != http.StatusNotFound {
		t.Errorf("putting on a missing sandbox = %d", put.StatusCode())
	}
	if d.Rules([16]byte{1}) != nil {
		t.Error("a missing sandbox has rules")
	}
}

// A sandbox's rules and refusals go with it, as the daemon's do: the next one
// made with the same rules' names starts with its own spec's and nothing the
// last one was given or refused.
func TestTheFakesRulesGoWithTheirSandbox(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("k"), WithNetwork())
	client := typed(t, d, "k")
	create := func(key string, rules ...genv1.EgressRule) genv1.SandboxID {
		t.Helper()
		created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: key},
			genv1.SandboxSpec{Image: "alpine:3.20", Egress: &genv1.EgressSpec{Rules: &rules}})
		if err != nil || created.JSON202 == nil {
			t.Fatalf("create = %v %s", err, created.Body)
		}
		return created.JSON202.Id
	}
	domains := []string{"first.example"}
	first := create("first", genv1.EgressRule{Name: "shared", Domains: &domains})
	added := []string{"added.example"}
	if put, _ := client.PutEgressRuleWithResponse(ctx, DefaultNamespace, first, "added", genv1.EgressRuleSpec{Domains: &added}); put.JSON201 == nil {
		t.Fatalf("adding = %d %s", put.StatusCode(), put.Body)
	}
	d.RecordRefusal(first, genv1.EgressRefusal{Target: "refused.example", At: time.Now()})
	if del, _ := client.DeleteSandboxWithResponse(ctx, DefaultNamespace, first, nil); del.StatusCode() != http.StatusNoContent {
		t.Fatalf("delete = %d %s", del.StatusCode(), del.Body)
	}

	others := []string{"second.example"}
	second := create("second", genv1.EgressRule{Name: "shared", Domains: &others})
	if rules := d.Rules(second); len(rules) != 1 || rules[0].Name != "shared" || (*rules[0].Domains)[0] != "second.example" {
		t.Fatalf("the next sandbox has %+v", rules)
	}
	report, _ := client.GetSandboxEgressWithResponse(ctx, DefaultNamespace, second)
	if report.JSON200 == nil || len(report.JSON200.Refusals) != 0 || len(*report.JSON200.Policy.Rules) != 1 {
		t.Fatalf("the next sandbox's report = %s", report.Body)
	}

	// The last one's are nobody's: not read, not changed, not recorded.
	if d.Rules(first) != nil {
		t.Errorf("the deleted sandbox still has rules %+v", d.Rules(first))
	}
	d.RecordRefusal(first, genv1.EgressRefusal{Target: "late.example", At: time.Now()})
	if got, _ := client.ListEgressRulesWithResponse(ctx, DefaultNamespace, first); got.StatusCode() != http.StatusNotFound {
		t.Errorf("listing the deleted sandbox's rules = %d", got.StatusCode())
	}
	if got, _ := client.GetEgressRuleWithResponse(ctx, DefaultNamespace, first, "added"); got.StatusCode() != http.StatusNotFound {
		t.Errorf("reading the deleted sandbox's rule = %d", got.StatusCode())
	}
	if got, _ := client.PutEgressRuleWithResponse(ctx, DefaultNamespace, first, "late", genv1.EgressRuleSpec{Domains: &added}); got.StatusCode() != http.StatusNotFound {
		t.Errorf("putting a rule on the deleted sandbox = %d", got.StatusCode())
	}
	if got, _ := client.DeleteEgressRuleWithResponse(ctx, DefaultNamespace, first, "shared"); got.StatusCode() != http.StatusNotFound {
		t.Errorf("deleting the deleted sandbox's rule = %d", got.StatusCode())
	}
	if report, _ := client.GetSandboxEgressWithResponse(ctx, DefaultNamespace, second); report.JSON200 == nil || len(report.JSON200.Refusals) != 0 {
		t.Errorf("a refusal recorded for the deleted sandbox reached the next: %s", report.Body)
	}
}

// Paused and resumed as the daemon does it: by name, kept in place, answered
// as it is now, refused where the daemon refuses.
func TestTheFakePausesAndResumesRules(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("k"), WithNetwork())
	client := typed(t, d, "k")
	cidrs := []string{"192.0.2.0/24"}
	rules := []genv1.EgressRule{{Name: "a", Cidrs: &cidrs}, {Name: "b", Cidrs: &cidrs}}
	created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "p"},
		genv1.SandboxSpec{Image: "alpine:3.20", Egress: &genv1.EgressSpec{Rules: &rules}})
	if err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}
	id := created.JSON202.Id
	for range 2 {
		if got, _ := client.PauseEgressRuleWithResponse(ctx, DefaultNamespace, id, "a"); got.JSON200 == nil || got.JSON200.Paused == nil || !*got.JSON200.Paused {
			t.Fatalf("pausing = %d %s", got.StatusCode(), got.Body)
		}
	}
	if got := d.Rules(id); got[0].Name != "a" || got[0].Paused == nil || got[1].Paused != nil {
		t.Fatalf("after pausing: %+v", got)
	}
	for range 2 {
		if got, _ := client.ResumeEgressRuleWithResponse(ctx, DefaultNamespace, id, "a"); got.JSON200 == nil || got.JSON200.Paused != nil {
			t.Fatalf("resuming = %d %s", got.StatusCode(), got.Body)
		}
	}
	for _, c := range []struct {
		rule   string
		status int
	}{{"nope", http.StatusNotFound}, {"Not-A-Name", http.StatusBadRequest}} {
		if got, _ := client.PauseEgressRuleWithResponse(ctx, DefaultNamespace, id, c.rule); got.StatusCode() != c.status {
			t.Errorf("pausing %q = %d", c.rule, got.StatusCode())
		}
		if got, _ := client.ResumeEgressRuleWithResponse(ctx, DefaultNamespace, id, c.rule); got.StatusCode() != c.status {
			t.Errorf("resuming %q = %d", c.rule, got.StatusCode())
		}
	}
	d.Settle(id, genv1.SandboxStateStopped, "")
	if got, _ := client.PauseEgressRuleWithResponse(ctx, DefaultNamespace, id, "a"); got.StatusCode() != http.StatusConflict {
		t.Errorf("pausing on a stopped sandbox = %d", got.StatusCode())
	}
}

// Killed as the daemon does it: what matches everything named, answered with
// what was killed and gone from the report; nothing named is refused.
func TestTheFakeKillsConnections(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("k"), WithNetwork())
	client := typed(t, d, "k")
	created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "c"}, genv1.SandboxSpec{Image: "alpine:3.20"})
	if err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}
	id := created.JSON202.Id
	d.OpenConnection(id, genv1.Connection{Protocol: "tcp", Source: "172.30.0.5:1", Destination: "192.0.2.1:443"})
	d.OpenConnection(id, genv1.Connection{Protocol: "udp", Source: "172.30.0.5:2", Destination: "192.0.2.1:53"})
	d.OpenConnection(genv1.SandboxID{}, genv1.Connection{Protocol: "tcp", Source: "x", Destination: "y"})
	destination := "192.0.2.1:443"
	got, _ := client.KillConnectionsWithResponse(ctx, DefaultNamespace, id, genv1.KillConnections{Destination: &destination})
	if got.JSON200 == nil || len(got.JSON200.Killed) != 1 || got.JSON200.Killed[0].Protocol != "tcp" {
		t.Fatalf("= %d %s", got.StatusCode(), got.Body)
	}
	report, _ := client.GetSandboxEgressWithResponse(ctx, DefaultNamespace, id)
	if report.JSON200 == nil || len(report.JSON200.Connections) != 1 || report.JSON200.Connections[0].Protocol != "udp" {
		t.Fatalf("the report = %s", report.Body)
	}
	if got, _ := client.KillConnectionsWithResponse(ctx, DefaultNamespace, id, genv1.KillConnections{}); got.StatusCode() != http.StatusBadRequest {
		t.Fatalf("naming neither = %d", got.StatusCode())
	}
	for _, raw := range []string{`{"destination": "192.0.2.1", "all": true}`, `{`} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL+"/v1/namespaces/default/sandboxes/"+id.String()+"/egress/connections/kill", strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d", raw, res.StatusCode)
		}
	}
	source, protocol := "172.30.0.5:2", "udp"
	if got, _ := client.KillConnectionsWithResponse(ctx, DefaultNamespace, id, genv1.KillConnections{Source: &source, Protocol: &protocol}); got.JSON200 == nil || len(got.JSON200.Killed) != 1 {
		t.Fatalf("by source = %s", got.Body)
	}
	if got, _ := client.KillConnectionsWithResponse(ctx, DefaultNamespace, genv1.SandboxID{}, genv1.KillConnections{Source: &source}); got.StatusCode() != http.StatusNotFound {
		t.Fatalf("no such sandbox = %d", got.StatusCode())
	}
}

// A sandbox that names no volume is given one, which its spec names: kept
// when it is dropped, and the next sandbox's that names it.
func TestASandboxThatNamesNoVolumeIsGivenOne(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("k"))
	client := typed(t, d, "k")
	created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "1"}, genv1.SandboxSpec{Image: "alpine:3.20"})
	if err != nil || created.JSON202 == nil || created.JSON202.Spec.Disk == nil || created.JSON202.Spec.Disk.Volume == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}
	volume := *created.JSON202.Spec.Disk.Volume
	if !strings.HasPrefix(volume, "vol_") {
		t.Fatalf("the volume given is %q", volume)
	}
	if exists, holder := d.Volume(volume); !exists || holder == nil || *holder != created.JSON202.Id {
		t.Fatalf("its volume: exists %v, held by %v", exists, holder)
	}
	if got, _ := client.DeleteSandboxWithResponse(ctx, DefaultNamespace, created.JSON202.Id, &genv1.DeleteSandboxParams{}); got.StatusCode() != 204 {
		t.Fatalf("delete = %d", got.StatusCode())
	}
	next, _ := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "2"}, genv1.SandboxSpec{Image: "alpine:3.20", Disk: &genv1.DiskSpec{Volume: &volume}})
	if next.JSON202 == nil {
		t.Fatalf("naming it = %d %s", next.StatusCode(), next.Body)
	}
	if _, holder := d.Volume(volume); holder == nil || *holder != next.JSON202.Id {
		t.Fatalf("it is held by %v", holder)
	}
}

// The double adds up the specs it was given, as the daemon does, and counts
// what it holds.
func TestTheFakeCountsAndAddsUpItsSandboxes(t *testing.T) {
	d := New(t, WithBrokers("claude"))
	// A volume no sandbox holds is promised all the same.
	d.PutVolume("vol_kept", 1<<30)
	client := typed(t, d, "")
	ctx := context.Background()
	cpus, memory, disk, odd := 2, "512Mi", "10Gi", "1.5G"
	for i, spec := range []genv1.SandboxSpec{
		{Image: "alpine", Cpus: &cpus, Memory: &memory, Disk: &genv1.DiskSpec{Size: &disk}},
		// What the double cannot read, or was not given, is nothing.
		{Image: "alpine", Memory: &odd, Disk: &genv1.DiskSpec{Size: &odd}},
		{Image: "alpine", Memory: new("xGi")},
	} {
		if created, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: strconv.Itoa(i)}, spec); err != nil || created.JSON202 == nil {
			t.Fatalf("create = %v %s", err, created.Body)
		}
	}
	promised, err := client.GetHostCommitmentsWithResponse(ctx)
	if err != nil || promised.JSON200 == nil {
		t.Fatalf("commitments = %v %s", err, promised.Body)
	}
	want := genv1.HostCommitments{Running: 3, Cpus: 2, MemoryBytes: 512 << 20, Volumes: 4, DiskBytes: 10<<30 + 1<<30}
	if *promised.JSON200 != want {
		t.Errorf("commitments = %+v, want %+v", *promised.JSON200, want)
	}
	counted, err := client.GetCountsWithResponse(ctx, DefaultNamespace)
	if err != nil || counted.JSON200 == nil || *counted.JSON200.Sandboxes != 3 || *counted.JSON200.Brokers != 1 ||
		*counted.JSON200.Images != 0 || *counted.JSON200.Tunnels != 0 || *counted.JSON200.Volumes != 4 {
		t.Errorf("counts = %v %s", err, counted.Body)
	}
}

// A sandbox stopped, started, labelled and found by its labels, as the
// daemon does each.
func TestTheFakeStopsStartsAndLabelsASandbox(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("k"))
	client := typed(t, d, "k")
	labels := map[string]string{"kind": "deployment", "owner": "ada"}
	made, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "one"}, genv1.SandboxSpec{Image: "alpine", Labels: &labels})
	if err != nil || made.JSON202 == nil {
		t.Fatalf("create = %v %s", err, made.Body)
	}
	if _, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "two"}, genv1.SandboxSpec{Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	id := made.JSON202.Id

	listed, err := client.ListSandboxesWithResponse(ctx, DefaultNamespace, &genv1.ListSandboxesParams{Label: &[]string{"kind=deployment", "owner=ada"}})
	if err != nil || listed.JSON200 == nil || len(listed.JSON200.Items) != 1 || listed.JSON200.Items[0].Id != id {
		t.Fatalf("the labelled ones = %v %s", err, listed.Body)
	}
	if listed, _ := client.ListSandboxesWithResponse(ctx, DefaultNamespace, &genv1.ListSandboxesParams{Label: &[]string{"owner"}}); listed.StatusCode() != http.StatusBadRequest {
		t.Errorf("a filter without = answered %d", listed.StatusCode())
	}
	if listed, _ := client.ListSandboxesWithResponse(ctx, DefaultNamespace, &genv1.ListSandboxesParams{}); listed.JSON200 == nil || len(listed.JSON200.Items) != 2 {
		t.Errorf("every one = %s", listed.Body)
	}

	if started, _ := client.StartSandboxWithResponse(ctx, DefaultNamespace, id); started.StatusCode() != http.StatusConflict {
		t.Errorf("starting a running sandbox = %d", started.StatusCode())
	}
	stopped, err := client.StopSandboxWithResponse(ctx, DefaultNamespace, id)
	if err != nil || stopped.JSON202 == nil || stopped.JSON202.State != genv1.SandboxStateStopped {
		t.Fatalf("stop = %v %s", err, stopped.Body)
	}
	if again, _ := client.StopSandboxWithResponse(ctx, DefaultNamespace, id); again.JSON202 == nil || again.JSON202.State != genv1.SandboxStateStopped {
		t.Errorf("stopping a stopped sandbox = %s", again.Body)
	}
	started, err := client.StartSandboxWithResponse(ctx, DefaultNamespace, id)
	if err != nil || started.JSON202 == nil || started.JSON202.State != genv1.SandboxStateReady {
		t.Fatalf("start = %v %s", err, started.Body)
	}

	set, err := client.SetSandboxLabelsWithResponse(ctx, DefaultNamespace, id, genv1.SetSandboxLabelsJSONRequestBody{Labels: map[string]string{"kind": "other"}})
	if err != nil || set.StatusCode() != http.StatusNoContent {
		t.Fatalf("labels = %v %d", err, set.StatusCode())
	}
	got, _ := client.GetSandboxWithResponse(ctx, DefaultNamespace, id)
	if got.JSON200 == nil || got.JSON200.Labels == nil || (*got.JSON200.Labels)["kind"] != "other" || (*got.JSON200.Labels)["owner"] != "" {
		t.Errorf("labelled = %s", got.Body)
	}
	if set, _ := client.SetSandboxLabelsWithBodyWithResponse(ctx, DefaultNamespace, id, "application/json", strings.NewReader(`{}`)); set.StatusCode() != http.StatusBadRequest {
		t.Errorf("no labels = %d", set.StatusCode())
	}
	missing := genv1.SandboxID{}
	for name, status := range map[string]int{
		"stop":   statusOf(client.StopSandboxWithResponse(ctx, DefaultNamespace, missing)),
		"start":  statusOf(client.StartSandboxWithResponse(ctx, DefaultNamespace, missing)),
		"labels": statusOf(client.SetSandboxLabelsWithResponse(ctx, DefaultNamespace, missing, genv1.SetSandboxLabelsJSONRequestBody{Labels: map[string]string{}})),
	} {
		if status != http.StatusNotFound {
			t.Errorf("%s of a sandbox that is not there = %d", name, status)
		}
	}
}

func statusOf[R interface{ StatusCode() int }](r R, err error) int {
	if err != nil {
		return 0
	}
	return r.StatusCode()
}

// A sealed sandbox refuses what runs or writes, and its processes' output
// is read as any sandbox's.
func TestTheFakeHoldsASealedSandboxSealedAndServesItsOutput(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("k"))
	client := typed(t, d, "k")
	made, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "s"}, genv1.SandboxSpec{Image: "alpine", Sealed: new(true)})
	if err != nil || made.JSON202 == nil {
		t.Fatalf("create = %v %s", err, made.Body)
	}
	id := made.JSON202.Id

	run, _ := client.RunCommandWithResponse(ctx, DefaultNamespace, id, genv1.CommandRequest{Argv: []string{"true"}})
	write, _ := client.WriteFileWithBodyWithResponse(ctx, DefaultNamespace, id, &genv1.WriteFileParams{Path: "/x"}, "application/octet-stream", strings.NewReader("x"))
	for name, body := range map[string][]byte{"command": run.Body, "file": write.Body} {
		if !strings.Contains(string(body), `"sealed"`) {
			t.Errorf("a %s on a sealed sandbox = %s", name, body)
		}
	}
	if len(d.Commands()) != 0 {
		t.Errorf("commands ran: %v", d.Commands())
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.URL+"/v1/namespaces/default/sandboxes/"+id.String()+"/terminal", http.NoBody)
	req.Header.Set("Authorization", "Bearer k")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a terminal on a sealed sandbox = %d", res.StatusCode)
	}

	if got, _ := client.GetSandboxWithResponse(ctx, DefaultNamespace, id); got.JSON200.Tainted != nil {
		t.Errorf("a refused call tainted it: %+v", got.JSON200.Tainted)
	}

	// Unsealed, the first call taints it, and sealing it again keeps that.
	unsealed, err := client.UnsealSandboxWithResponse(ctx, DefaultNamespace, id)
	if err != nil || unsealed.JSON200 == nil || *unsealed.JSON200.Spec.Sealed || unsealed.JSON200.Tainted != nil {
		t.Fatalf("unseal = %v %s", err, unsealed.Body)
	}
	if run, _ := client.RunCommandWithResponse(ctx, DefaultNamespace, id, genv1.CommandRequest{Argv: []string{"true"}}); run.StatusCode() != http.StatusOK {
		t.Fatalf("a command once unsealed = %d", run.StatusCode())
	}
	_, _ = client.WriteFileWithBodyWithResponse(ctx, DefaultNamespace, id, &genv1.WriteFileParams{Path: "/y"}, "application/octet-stream", strings.NewReader("y"))
	resealed, err := client.SealSandboxWithResponse(ctx, DefaultNamespace, id)
	if err != nil || resealed.JSON200 == nil || !*resealed.JSON200.Spec.Sealed {
		t.Fatalf("seal = %v %s", err, resealed.Body)
	}
	taint := resealed.JSON200.Tainted
	if taint == nil || taint.Route != "POST /v1/namespaces/{namespace}/sandboxes/{sandboxId}/commands" || *taint.By.KeyId != "koperator" {
		t.Errorf("tainted %+v", taint)
	}
	if run, _ := client.RunCommandWithResponse(ctx, DefaultNamespace, id, genv1.CommandRequest{Argv: []string{"true"}}); run.StatusCode() != http.StatusForbidden {
		t.Errorf("a command once sealed again = %d", run.StatusCode())
	}
	onVolume, _ := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: "v"}, genv1.SandboxSpec{Image: "alpine", Disk: &genv1.DiskSpec{Volume: new(genv1.VolumeName("work"))}})
	if got, _ := client.SealSandboxWithResponse(ctx, DefaultNamespace, onVolume.JSON202.Id); got.StatusCode() != http.StatusConflict {
		t.Errorf("sealing one on a volume = %d", got.StatusCode())
	}
	if got, _ := client.UnsealSandboxWithResponse(ctx, DefaultNamespace, genv1.SandboxID{}); got.StatusCode() != http.StatusNotFound {
		t.Errorf("unsealing one that is not there = %d", got.StatusCode())
	}

	d.SetOutput(id, "entrypoint", "stdout", "listening on :8080\n")
	d.SetOutput(id, "entrypoint", "stderr", "warning\n")
	d.SetOutput(genv1.SandboxID{}, "entrypoint", "stdout", "nobody")
	from := int64(13)
	out, err := client.ReadProcessStdoutWithResponse(ctx, DefaultNamespace, id, "entrypoint", &genv1.ReadProcessStdoutParams{From: &from})
	if err != nil || string(out.Body) != ":8080\n" {
		t.Errorf("stdout from 13 = %v %q", err, out.Body)
	}
	past := int64(100)
	if out, _ := client.ReadProcessStdoutWithResponse(ctx, DefaultNamespace, id, "entrypoint", &genv1.ReadProcessStdoutParams{From: &past}); out.StatusCode() != http.StatusOK || len(out.Body) != 0 {
		t.Errorf("stdout past its end = %d %q", out.StatusCode(), out.Body)
	}
	errs, _ := client.ReadProcessStderrWithResponse(ctx, DefaultNamespace, id, "entrypoint", &genv1.ReadProcessStderrParams{})
	if string(errs.Body) != "warning\n" {
		t.Errorf("stderr = %q", errs.Body)
	}
	if out, _ := client.ReadProcessStdoutWithResponse(ctx, DefaultNamespace, id, "other", &genv1.ReadProcessStdoutParams{}); out.StatusCode() != http.StatusNotFound {
		t.Errorf("a process that is not there = %d", out.StatusCode())
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, d.URL+"/v1/namespaces/default/sandboxes/"+id.String()+"/processes/entrypoint/stdout?from=x", http.NoBody)
	req.Header.Set("Authorization", "Bearer k")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusBadRequest {
		t.Errorf("from=x = %v %v", err, res)
	} else {
		_ = res.Body.Close()
	}
	if out, _ := client.ReadProcessStdoutWithResponse(ctx, DefaultNamespace, genv1.SandboxID{}, "entrypoint", &genv1.ReadProcessStdoutParams{}); out.StatusCode() != http.StatusNotFound {
		t.Errorf("a sandbox that is not there = %d", out.StatusCode())
	}
}
