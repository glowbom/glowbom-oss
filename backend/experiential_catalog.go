package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type experientialModelAnnotation struct {
	Free            bool
	RecommendedRank *int
}

type experientialCatalogModel struct {
	Model struct {
		Slug          string `json:"slug"`
		Status        string `json:"status"`
		PreferredRank *int   `json:"preferred_rank"`
	} `json:"model"`
	PromotionalListed bool `json:"promotional_listed"`
}

type experientialCatalogPromotion struct {
	Free                  *bool             `json:"free"`
	DisplayOnly           *bool             `json:"display_only"`
	RequiresPayment       *bool             `json:"requires_payment"`
	RequiresPaymentMethod *bool             `json:"requires_payment_method"`
	Slugs                 []string          `json:"slugs"`
	ListedSlugs           []string          `json:"listed_slugs"`
	Providers             []json.RawMessage `json:"providers"`
	FamilyKeys            []json.RawMessage `json:"family_keys"`
	ModelTerms            []json.RawMessage `json:"model_terms"`
	TokenRates            []json.RawMessage `json:"token_rates"`
}

func (p experientialCatalogPromotion) unrestrictedFree() bool {
	return p.Free != nil && *p.Free && p.DisplayOnly != nil && !*p.DisplayOnly &&
		p.RequiresPayment != nil && !*p.RequiresPayment &&
		p.RequiresPaymentMethod != nil && !*p.RequiresPaymentMethod &&
		len(p.Providers) == 0 && len(p.FamilyKeys) == 0 && len(p.ModelTerms) == 0 && len(p.TokenRates) == 0
}

// Public catalog labels describe the current listing, not an account's billing terms.
// Fetch them independently from OpenCode model registration and never send a key.
func loadExperientialModelAnnotations(ctx context.Context, client *http.Client) (map[string]experientialModelAnnotation, error) {
	models := map[string]experientialCatalogModel{}
	freeSlugs := map[string]bool{}
	for offset := 0; offset < 10000; {
		endpoint := fmt.Sprintf("https://api.experientiallabs.ai/api/models?sort=preferred&limit=1000&offset=%d", offset)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, errors.New("could not request public model catalog")
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, errors.New("could not reach public model catalog")
		}
		var catalog struct {
			Models     []experientialCatalogModel     `json:"models"`
			Promotions []experientialCatalogPromotion `json:"promotions"`
			Total      *int                           `json:"total"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&catalog)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil || catalog.Total == nil || *catalog.Total < 0 {
			return nil, errors.New("public model catalog unavailable")
		}
		for _, entry := range catalog.Models {
			if entry.Model.Slug != "" && entry.Model.Status == "active" {
				models[entry.Model.Slug] = entry
			}
		}
		for _, promotion := range catalog.Promotions {
			if !promotion.unrestrictedFree() {
				continue
			}
			for _, slug := range promotion.ListedSlugs {
				if len(promotion.Slugs) == 0 || slices.Contains(promotion.Slugs, slug) {
					freeSlugs[slug] = true
				}
			}
		}
		offset += len(catalog.Models)
		if offset >= *catalog.Total {
			annotations := make(map[string]experientialModelAnnotation, len(models))
			for slug, entry := range models {
				rank := entry.Model.PreferredRank
				if rank != nil && *rank < 0 {
					rank = nil
				}
				annotations[slug] = experientialModelAnnotation{
					Free: entry.PromotionalListed && freeSlugs[slug], RecommendedRank: rank,
				}
			}
			return annotations, nil
		}
		if len(catalog.Models) == 0 {
			break
		}
	}
	return nil, errors.New("incomplete public model catalog")
}

type experientialModelCatalogCache struct {
	mu          sync.Mutex
	annotations map[string]experientialModelAnnotation
	nextLoad    time.Time
	loading     chan struct{}
	fetch       func(context.Context) (map[string]experientialModelAnnotation, error)
	now         func() time.Time
	timeout     time.Duration
}

func newExperientialModelCatalogCache() *experientialModelCatalogCache {
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &experientialModelCatalogCache{
		fetch: func(ctx context.Context) (map[string]experientialModelAnnotation, error) {
			return loadExperientialModelAnnotations(ctx, client)
		},
		now: time.Now, timeout: 3 * time.Second,
	}
}

func (c *experientialModelCatalogCache) get(ctx context.Context) (map[string]experientialModelAnnotation, bool) {
	for {
		c.mu.Lock()
		if c.now().Before(c.nextLoad) {
			annotations := c.annotations
			c.mu.Unlock()
			return annotations, annotations != nil
		}
		if pending := c.loading; pending != nil {
			c.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, false
			}
		}
		if ctx.Err() != nil {
			c.mu.Unlock()
			return nil, false
		}
		c.loading = make(chan struct{})
		c.mu.Unlock()
		loadCtx, cancel := context.WithTimeout(ctx, c.timeout)
		annotations, err := c.fetch(loadCtx)
		cancel()
		c.mu.Lock()
		if err != nil {
			// Expired promotions cannot keep their Free label after a failed refresh.
			c.annotations = nil
			c.nextLoad = c.now().Add(30 * time.Second)
		} else {
			c.annotations = annotations
			c.nextLoad = c.now().Add(5 * time.Minute)
		}
		close(c.loading)
		c.loading = nil
		result := c.annotations
		c.mu.Unlock()
		return result, result != nil
	}
}

func (s *chatService) annotateExperientialModels(ctx context.Context, models []chatModel) []chatModel {
	if s.experientialCatalog == nil || !slices.ContainsFunc(models, func(model chatModel) bool {
		return strings.HasPrefix(model.ID, "explabs/")
	}) {
		return models
	}
	annotations, ok := s.experientialCatalog.get(ctx)
	if !ok {
		return models
	}
	result := slices.Clone(models)
	for i := range result {
		if !strings.HasPrefix(result[i].ID, "explabs/") {
			continue
		}
		annotation, found := annotations[strings.TrimPrefix(result[i].ID, "explabs/")]
		if !found {
			continue
		}
		free := annotation.Free
		result[i].Free = &free
		result[i].RecommendedRank = annotation.RecommendedRank
	}
	return result
}
