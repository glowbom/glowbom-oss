package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func experientialCatalogEntry(slug string, rank any, promotional bool) map[string]any {
	return map[string]any{
		"model":              map[string]any{"slug": slug, "status": "active", "preferred_rank": rank},
		"promotional_listed": promotional,
	}
}

func experientialFreePromotion(slug string) map[string]any {
	return map[string]any{
		"free": true, "display_only": false, "requires_payment": false,
		"requires_payment_method": false, "slugs": []string{slug}, "listed_slugs": []string{slug},
	}
}

func experientialPublicCatalogClient(t *testing.T, pages map[int]any) *http.Client {
	t.Helper()
	return &http.Client{Transport: experientialTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "api.experientiallabs.ai" || r.URL.Path != "/api/models" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("public catalog must use the expected endpoint without credentials")
		}
		if r.URL.Query().Get("sort") != "preferred" || r.URL.Query().Get("limit") != "1000" {
			t.Error("incorrect public catalog query")
		}
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil {
			t.Fatal(err)
		}
		page, ok := pages[offset]
		if !ok {
			t.Fatalf("unexpected catalog page %d", offset)
		}
		body, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
}

func TestExperientialPublicCatalogLabelsAndPagination(t *testing.T) {
	client := experientialPublicCatalogClient(t, map[int]any{
		0: map[string]any{
			"total": 4,
			"models": []any{
				experientialCatalogEntry("recommended", 0, false),
				experientialCatalogEntry("free-model", nil, true),
			},
			"promotions": []any{experientialFreePromotion("free-model")},
		},
		2: map[string]any{
			"total": 4,
			"models": []any{
				experientialCatalogEntry("discounted", 4, true),
				experientialCatalogEntry("ordinary", -1, false),
			},
			"promotions": []any{map[string]any{
				"free": false, "badge_style": "free", "slugs": []string{"discounted"},
				"listed_slugs": []string{"discounted"}, "discount": 0.5,
			}},
		},
	})
	annotations, err := loadExperientialModelAnnotations(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) != 4 || !annotations["free-model"].Free || annotations["discounted"].Free || annotations["ordinary"].RecommendedRank != nil {
		t.Fatal("incorrect free or recommendation labels")
	}
	if rank := annotations["recommended"].RecommendedRank; rank == nil || *rank != 0 {
		t.Fatal("recommended rank zero was discarded")
	}
	if rank := annotations["discounted"].RecommendedRank; rank == nil || *rank != 4 {
		t.Fatal("a discount must not hide a recommended model")
	}
}

func TestExperientialPublicCatalogRejectsRestrictedFreeClaims(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(map[string]any)
		promotional bool
		wantFree    bool
	}{
		{"unrestricted", func(map[string]any) {}, true, true},
		{"optional slugs", func(p map[string]any) { delete(p, "slugs") }, true, true},
		{"paid discount", func(p map[string]any) { p["free"] = false; p["badge_style"] = "free" }, true, false},
		{"payment required", func(p map[string]any) { p["requires_payment"] = true }, true, false},
		{"payment method required", func(p map[string]any) { p["requires_payment_method"] = true }, true, false},
		{"display only", func(p map[string]any) { p["display_only"] = true }, true, false},
		{"missing free", func(p map[string]any) { delete(p, "free") }, true, false},
		{"missing payment gate", func(p map[string]any) { delete(p, "requires_payment") }, true, false},
		{"missing payment method gate", func(p map[string]any) { delete(p, "requires_payment_method") }, true, false},
		{"missing display gate", func(p map[string]any) { delete(p, "display_only") }, true, false},
		{"not currently listed", func(p map[string]any) { delete(p, "listed_slugs") }, true, false},
		{"outside promotion scope", func(p map[string]any) { p["slugs"] = []string{"another-model"} }, true, false},
		{"not in promotional group", func(map[string]any) {}, false, false},
		{"provider restriction", func(p map[string]any) { p["providers"] = []string{"provider"} }, true, false},
		{"family restriction", func(p map[string]any) { p["family_keys"] = []string{"family"} }, true, false},
		{"model terms", func(p map[string]any) { p["model_terms"] = []any{map[string]any{"limit": 100}} }, true, false},
		{"token rate restriction", func(p map[string]any) { p["token_rates"] = []any{map[string]any{"limit": 100}} }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			promotion := experientialFreePromotion("model")
			tc.change(promotion)
			client := experientialPublicCatalogClient(t, map[int]any{0: map[string]any{
				"total": 1, "models": []any{experientialCatalogEntry("model", nil, tc.promotional)}, "promotions": []any{promotion},
			}})
			annotations, err := loadExperientialModelAnnotations(context.Background(), client)
			if err != nil || annotations["model"].Free != tc.wantFree {
				t.Fatal("incorrect free label", annotations, err)
			}
		})
	}
}

func TestExperientialPublicCatalogFailureDropsPartialLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"upstream failure", 503, `{"total":2,"models":[]}`},
		{"malformed response", 200, `not json`},
		{"missing total", 200, `{"models":[]}`},
		{"negative total", 200, `{"total":-1,"models":[]}`},
		{"incomplete page", 200, `{"total":2,"models":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: experientialTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				status, body := 200, `{"total":2,"models":[{"model":{"slug":"model","status":"active","preferred_rank":0}}]}`
				if calls == 2 {
					status, body = tc.status, tc.body
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			annotations, err := loadExperientialModelAnnotations(context.Background(), client)
			if err == nil || annotations != nil {
				t.Fatal("partial metadata must not survive a failed catalog refresh")
			}
		})
	}
}

func TestExperientialModelAnnotationsPreserveDiscoveredCatalog(t *testing.T) {
	rank := 0
	models := []chatModel{
		{ID: "explabs/free-model", Name: "Free model", Provider: "Experiential Labs", Images: true, Build: true},
		{ID: "explabs/recommended", Name: "Recommended", Build: true},
		{ID: "explabs/ordinary", Name: "Ordinary"},
		{ID: "explabs/vendor/free-model", Name: "Unknown alias"},
		{ID: "explabs/missing", Name: "Custom model"},
		{ID: "other/free-model", Name: "Another provider"},
	}
	service := &chatService{experientialCatalog: &experientialModelCatalogCache{
		now: time.Now, timeout: time.Second,
		fetch: func(context.Context) (map[string]experientialModelAnnotation, error) {
			return map[string]experientialModelAnnotation{
				"free-model": {Free: true}, "recommended": {RecommendedRank: &rank}, "ordinary": {}, "undiscovered": {Free: true},
			}, nil
		},
	}}
	result := service.annotateExperientialModels(context.Background(), models)
	if len(result) != len(models) || result[0].Free == nil || !*result[0].Free || result[1].RecommendedRank == nil || *result[1].RecommendedRank != 0 || result[2].Free == nil || *result[2].Free {
		t.Fatal("incorrect discovered model annotations")
	}
	for i := range result {
		plain := result[i]
		plain.Free, plain.RecommendedRank = nil, nil
		if !reflect.DeepEqual(plain, models[i]) {
			t.Fatal("catalog annotation changed existing model selection or capabilities")
		}
		if i >= 3 && (result[i].Free != nil || result[i].RecommendedRank != nil) {
			t.Fatal("unknown aliases, missing rows, and other providers must stay unannotated")
		}
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), `"free":false`) || !strings.Contains(string(data), `"recommendedRank":0`) || models[0].Free != nil {
		t.Fatal("JSON contract lost explicit false or rank zero, or mutated the input")
	}
}

func TestExperientialCatalogCacheExpiryFailureAndRetry(t *testing.T) {
	now := time.Date(2026, time.October, 5, 0, 0, 0, 0, time.UTC)
	calls, fail := 0, false
	cache := &experientialModelCatalogCache{
		now: func() time.Time { return now }, timeout: time.Second,
		fetch: func(context.Context) (map[string]experientialModelAnnotation, error) {
			calls++
			if fail {
				return nil, errors.New("catalog unavailable")
			}
			return map[string]experientialModelAnnotation{"model": {Free: true}}, nil
		},
	}
	if labels, ok := cache.get(context.Background()); !ok || !labels["model"].Free {
		t.Fatal("initial metadata load failed")
	}
	now = now.Add(5*time.Minute - time.Second)
	if _, ok := cache.get(context.Background()); !ok || calls != 1 {
		t.Fatal("unexpired metadata did not stay cached")
	}
	now = now.Add(time.Second)
	fail = true
	if labels, ok := cache.get(context.Background()); ok || labels != nil || calls != 2 {
		t.Fatal("expired free claim survived a failed refresh")
	}
	now = now.Add(29 * time.Second)
	if _, ok := cache.get(context.Background()); ok || calls != 2 {
		t.Fatal("failed refresh was not throttled")
	}
	now = now.Add(time.Second)
	fail = false
	if _, ok := cache.get(context.Background()); !ok || calls != 3 {
		t.Fatal("metadata did not retry after the failure backoff")
	}
	if !cache.nextLoad.Equal(now.Add(5 * time.Minute)) {
		t.Fatal("successful refresh must start a new finite cache window")
	}
}

func TestExperientialCatalogCoalescesConcurrentLoads(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	cache := &experientialModelCatalogCache{
		now: time.Now, timeout: time.Second,
		fetch: func(context.Context) (map[string]experientialModelAnnotation, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return map[string]experientialModelAnnotation{"model": {Free: true}}, nil
		},
	}
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if labels, ok := cache.get(context.Background()); !ok || !labels["model"].Free {
				t.Error("concurrent caller did not receive metadata")
			}
		}()
	}
	<-started
	close(release)
	workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent requests fetched the catalog more than once")
	}
}

func TestExperientialCatalogCancelledWaiterDoesNotCancelSharedLoad(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	cache := &experientialModelCatalogCache{
		now: time.Now, timeout: time.Second,
		fetch: func(context.Context) (map[string]experientialModelAnnotation, error) {
			close(started)
			<-release
			return map[string]experientialModelAnnotation{"model": {}}, nil
		},
	}
	go func() { _, ok := cache.get(context.Background()); done <- ok }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := cache.get(ctx); ok {
		t.Fatal("cancelled waiter received a catalog")
	}
	close(release)
	if !<-done {
		t.Fatal("cancelled waiter interrupted the other caller")
	}
}

func TestExperientialAnnotationsFallbackWithoutUnnecessaryNetwork(t *testing.T) {
	models := []chatModel{{ID: "explabs/model", Name: "Model", Build: true}}
	service := &chatService{experientialCatalog: &experientialModelCatalogCache{
		now: time.Now, timeout: 5 * time.Millisecond,
		fetch: func(ctx context.Context) (map[string]experientialModelAnnotation, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}}
	if got := service.annotateExperientialModels(context.Background(), models); !reflect.DeepEqual(got, models) {
		t.Fatal("failed catalog annotation changed the usable OpenCode models")
	}
	service.experientialCatalog.fetch = func(context.Context) (map[string]experientialModelAnnotation, error) {
		t.Fatal("models from other providers must not fetch this gateway catalog")
		return nil, nil
	}
	otherModels := []chatModel{{ID: "openai/model"}}
	if got := service.annotateExperientialModels(context.Background(), otherModels); !reflect.DeepEqual(got, otherModels) {
		t.Fatal("other provider models changed")
	}
	if got := (&chatService{}).annotateExperientialModels(context.Background(), models); !reflect.DeepEqual(got, models) {
		t.Fatal("a service without catalog injection must stay network independent")
	}
}

func TestExperientialModelAvailabilityDoesNotFetchPublicAnnotations(t *testing.T) {
	t.Setenv("GLOWBOM_CURSOR_BIN", "/missing/cursor-test")
	rememberCursorModels(nil)
	service := &chatService{
		directory: "/isolated", serverURL: "http://fixture",
		client: &http.Client{Transport: experientialTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/provider" {
				t.Fatal("unexpected availability request", r.URL.Path)
			}
			body := `{"connected":["explabs"],"all":[{"id":"explabs","name":"Experiential Labs","models":{"model":{"name":"Model","tool_call":true}}}]}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
		experientialCatalog: &experientialModelCatalogCache{fetch: func(context.Context) (map[string]experientialModelAnnotation, error) {
			t.Fatal("chat and build availability must not depend on the public metadata service")
			return nil, nil
		}},
	}
	models, err := service.models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "explabs/model" || !models[0].Build || models[0].Free != nil || models[0].RecommendedRank != nil {
		t.Fatal("availability changed", models, err)
	}
}
