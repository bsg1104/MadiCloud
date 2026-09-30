package application_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"madicloud/internal/application"
	"madicloud/internal/application/applicationtest"
)

func TestValidateNameAccepts(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"abc",
		"my-app",
		"a1b",
		"app-2",
		"a-b-c",
		"x" + strings.Repeat("y", 61) + "z", // 63
		"abc123",
	} {
		if err := application.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v", name, err)
		}
	}
}

func TestValidateNameRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
	}{
		{"", "name is required"},
		{"ab", "between 3 and 63"},
		{"a" + strings.Repeat("b", 63), "between 3 and 63"}, // 64
		{"My-app", "only lowercase letters, digits, and hyphens"},
		{"my_app", "only lowercase letters, digits, and hyphens"},
		{"my.app", "only lowercase letters, digits, and hyphens"},
		{"my app", "only lowercase letters, digits, and hyphens"},
		{"appé", "only lowercase letters, digits, and hyphens"},
		{"app\x00x", "only lowercase letters, digits, and hyphens"},
		{"1app", "must start with a lowercase letter"},
		{"-app", "must start with a lowercase letter"},
		{"app-", "must end with a lowercase letter or digit"},
		{"my--app", "consecutive hyphens"},
		{"xn--abc", "consecutive hyphens"},
	}
	for _, tt := range tests {
		err := application.ValidateName(tt.name)
		var ve *application.ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Message, tt.want) {
			t.Errorf("ValidateName(%q) = %v, want %q", tt.name, err, tt.want)
			continue
		}
		if ve.Field != "name" {
			t.Errorf("Field = %q", ve.Field)
		}
		if tt.name != "" && strings.Contains(ve.Message, tt.name) {
			t.Errorf("message echoes input: %q", ve.Message)
		}
	}
}

func TestValidateNameBoundaries(t *testing.T) {
	t.Parallel()

	for n := 0; n <= 70; n++ {
		name := ""
		if n > 0 {
			name = "a" + strings.Repeat("b", n-1)
		}
		err := application.ValidateName(name)
		want := n >= application.NameMinLen && n <= application.NameMaxLen
		if (err == nil) != want {
			t.Errorf("len %d: err = %v, want valid=%v", n, err, want)
		}
	}
}

func TestParseID(t *testing.T) {
	t.Parallel()

	got, err := application.ParseID("3F2504E0-4F89-41D3-9A0C-0305E82C3301")
	if err != nil || got != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("ParseID = %q, %v", got, err)
	}
	for _, bad := range []string{
		"",
		"not-a-uuid",
		"3f2504e04f8941d39a0c0305e82c3301",
		"{3f2504e0-4f89-41d3-9a0c-0305e82c3301}",
		"urn:uuid:3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"3f2504e0-4f89-41d3-9a0c-0305e82c330g",
		"3f2504e0_4f89-41d3-9a0c-0305e82c3301",
		"3f2504e0-4f89-41d3-9a0c-0305e82c33011",
		"'; DROP TABLE applications; --------",
	} {
		if _, err := application.ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) accepted", bad)
		}
	}
}

func TestDesiredStateValid(t *testing.T) {
	t.Parallel()
	if !application.DesiredActive.Valid() || !application.DesiredDeleted.Valid() {
		t.Fatal("known states invalid")
	}
	if application.DesiredState("running").Valid() || application.DesiredState("").Valid() {
		t.Fatal("unknown state valid")
	}
}

func newService(t *testing.T) (*application.Service, *applicationtest.MemoryStore) {
	t.Helper()
	store := applicationtest.NewMemoryStore()
	svc, err := application.NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	return svc, store
}

func TestNewServiceRequiresStore(t *testing.T) {
	t.Parallel()
	if _, err := application.NewService(nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestServiceCreateValidates(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()

	var ve *application.ValidationError
	if _, err := svc.Create(ctx, application.CreateInput{Name: "Bad"}); !errors.As(err, &ve) {
		t.Fatalf("Create bad name = %v", err)
	}
	for _, key := range []string{strings.Repeat("k", 256), "has space", "tab\tkey", "ünï"} {
		if _, err := svc.Create(ctx, application.CreateInput{Name: "good-name", IdempotencyKey: key}); !errors.As(err, &ve) {
			t.Fatalf("Create with key %q = %v", key, err)
		}
	}
	if _, err := svc.Create(ctx, application.CreateInput{Name: "good-name", IdempotencyKey: strings.Repeat("k", 255)}); err != nil {
		t.Fatalf("255-char key rejected: %v", err)
	}
}

func TestServiceCreateAndGet(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()

	res, err := svc.Create(ctx, application.CreateInput{Name: "my-app"})
	if err != nil {
		t.Fatal(err)
	}
	app := res.Application
	if res.Replayed || app.Name != "my-app" || app.DesiredState != application.DesiredActive || app.ID == "" {
		t.Fatalf("created = %+v", res)
	}

	got, err := svc.Get(ctx, strings.ToUpper(app.ID))
	if err != nil || got.ID != app.ID {
		t.Fatalf("Get upper-case id = %+v, %v", got, err)
	}
	if got, err := svc.GetByName(ctx, "my-app"); err != nil || got.ID != app.ID {
		t.Fatalf("GetByName = %+v, %v", got, err)
	}
	if _, err := svc.Create(ctx, application.CreateInput{Name: "my-app"}); !errors.Is(err, application.ErrNameTaken) {
		t.Fatalf("duplicate = %v", err)
	}
}

func TestServiceNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()
	missing := "00000000-0000-4000-8000-000000000000"

	if _, err := svc.Get(ctx, missing); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("Get = %v", err)
	}
	if _, err := svc.GetByName(ctx, "nope-app"); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("GetByName = %v", err)
	}
	if _, err := svc.Delete(ctx, missing); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("Delete = %v", err)
	}
}

func TestServiceMalformedIDNeverReachesStore(t *testing.T) {
	t.Parallel()
	svc, store := newService(t)
	store.Err = errors.New("store must not be called")
	ctx := context.Background()

	var ve *application.ValidationError
	if _, err := svc.Get(ctx, "nope"); !errors.As(err, &ve) {
		t.Fatalf("Get = %v", err)
	}
	if _, err := svc.Delete(ctx, "nope"); !errors.As(err, &ve) {
		t.Fatalf("Delete = %v", err)
	}
	if _, err := svc.GetByName(ctx, "NOPE"); !errors.As(err, &ve) {
		t.Fatalf("GetByName = %v", err)
	}
}

func TestServiceDelete(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()

	res, err := svc.Create(ctx, application.CreateInput{Name: "gone-app"})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := svc.Delete(ctx, res.Application.ID)
	if err != nil || deleted.ID != res.Application.ID {
		t.Fatalf("Delete = %+v, %v", deleted, err)
	}
	if _, err := svc.Delete(ctx, res.Application.ID); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("second Delete = %v", err)
	}
	if _, err := svc.Create(ctx, application.CreateInput{Name: "gone-app"}); err != nil {
		t.Fatalf("name not reusable after delete: %v", err)
	}
}

func TestServiceListDeterministicPaging(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()

	names := []string{"delta", "alpha", "charlie", "bravo", "echo", "a-b", "ab1"}
	for _, n := range names {
		if _, err := svc.Create(ctx, application.CreateInput{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"a-b", "ab1", "alpha", "bravo", "charlie", "delta", "echo"}

	var got []string
	after := ""
	pages := 0
	for {
		page, err := svc.List(ctx, application.ListInput{Limit: 3, After: after})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, a := range page.Applications {
			got = append(got, a.Name)
		}
		if page.NextAfter == "" {
			break
		}
		after = page.NextAfter
	}
	if strings.Join(got, ",") != strings.Join(want, ",") || pages != 3 {
		t.Fatalf("paged = %v in %d pages", got, pages)
	}

	page, err := svc.List(ctx, application.ListInput{Limit: 7})
	if err != nil || len(page.Applications) != 7 || page.NextAfter != "" {
		t.Fatalf("exact-fit page = %d apps, next %q, %v", len(page.Applications), page.NextAfter, err)
	}
	page, err = svc.List(ctx, application.ListInput{})
	if err != nil || len(page.Applications) != 7 {
		t.Fatalf("default limit = %d, %v", len(page.Applications), err)
	}
}

func TestServiceListValidates(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)
	ctx := context.Background()
	var ve *application.ValidationError
	for _, in := range []application.ListInput{
		{Limit: -1},
		{Limit: application.MaxListLimit + 1},
		{After: "Not Valid"},
	} {
		if _, err := svc.List(ctx, in); !errors.As(err, &ve) {
			t.Errorf("List(%+v) = %v", in, err)
		}
	}
	if _, err := svc.List(ctx, application.ListInput{Limit: application.MaxListLimit}); err != nil {
		t.Fatalf("max limit rejected: %v", err)
	}
}

func TestServiceIdempotentCreate(t *testing.T) {
	t.Parallel()
	svc, store := newService(t)
	ctx := context.Background()

	first, err := svc.Create(ctx, application.CreateInput{Name: "idem-app", IdempotencyKey: "k-1"})
	if err != nil || first.Replayed {
		t.Fatalf("first = %+v, %v", first, err)
	}
	again, err := svc.Create(ctx, application.CreateInput{Name: "idem-app", IdempotencyKey: "k-1"})
	if err != nil || !again.Replayed || again.Application != first.Application {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if _, err := svc.Create(ctx, application.CreateInput{Name: "other-app", IdempotencyKey: "k-1"}); !errors.Is(err, application.ErrIdempotencyKeyReused) {
		t.Fatalf("different request = %v", err)
	}

	// After the TTL the key is free again.
	store.Now = func() time.Time { return time.Now().Add(application.IdempotencyTTL + time.Minute) }
	res, err := svc.Create(ctx, application.CreateInput{Name: "other-app", IdempotencyKey: "k-1"})
	if err != nil || res.Replayed {
		t.Fatalf("after expiry = %+v, %v", res, err)
	}
}

func TestServiceConcurrentIdempotentCreate(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t)

	const n = 16
	var wg sync.WaitGroup
	results := make([]application.CreateResult, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.Create(context.Background(), application.CreateInput{Name: "race-app", IdempotencyKey: "same"})
		}()
	}
	wg.Wait()
	fresh := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if results[i].Application.ID != results[0].Application.ID {
			t.Fatal("different applications for one key")
		}
		if !results[i].Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d requests created, want 1", fresh)
	}
}

func TestServicePassesStoreErrors(t *testing.T) {
	t.Parallel()
	svc, store := newService(t)
	store.Err = application.ErrUnavailable
	if _, err := svc.Create(context.Background(), application.CreateInput{Name: "abc"}); !errors.Is(err, application.ErrUnavailable) {
		t.Fatalf("Create = %v", err)
	}
	if _, err := svc.List(context.Background(), application.ListInput{}); !errors.Is(err, application.ErrUnavailable) {
		t.Fatalf("List = %v", err)
	}
}
