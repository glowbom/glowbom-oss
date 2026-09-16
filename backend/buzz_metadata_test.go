// Created by Codex for Glowbom OSS.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const metadataOwnerKey = "0000000000000000000000000000000000000000000000000000000000000001"
const metadataAgentKey = "0000000000000000000000000000000000000000000000000000000000000002"
const metadataOtherKey = "0000000000000000000000000000000000000000000000000000000000000003"

func metadataEvent(t *testing.T, key string, kind int, tags nostr.Tags, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{Kind: kind, CreatedAt: nostr.Now() - 10, Tags: tags, Content: content}
	if err := event.Sign(key); err != nil {
		t.Fatal(err)
	}
	return event
}

func metadataAuth(t *testing.T, agent, conditions string) nostr.Tag {
	t.Helper()
	key, _ := hex.DecodeString(metadataOwnerKey)
	private, public := btcec.PrivKeyFromBytes(key)
	hash := sha256.Sum256([]byte("nostr:agent-auth:" + agent + ":" + conditions))
	sig, err := schnorr.Sign(private, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return nostr.Tag{"auth", hex.EncodeToString(schnorr.SerializePubKey(public)), conditions, hex.EncodeToString(sig.Serialize())}
}

func metadataFixture(t *testing.T) (nostr.Event, nostr.Event, nostr.Event) {
	t.Helper()
	key, _ := nostr.GetPublicKey(metadataAgentKey)
	profile := metadataEvent(t, metadataAgentKey, 0, nostr.Tags{metadataAuth(t, key, "kind=0")}, `{"name":"Ari","about":"An engineer"}`)
	instance := metadataEvent(t, metadataOwnerKey, 30177, nostr.Tags{{"d", key}}, `{"name":"Ari","persona_id":"engineer"}`)
	definition := metadataEvent(t, metadataOwnerKey, 30175, nostr.Tags{{"d", "engineer"}}, `{"display_name":"Engineer","runtime":"claude-code","model":"example-model","system_prompt":"must not reach client"}`)
	return profile, instance, definition
}

func TestBuzzRuntimeMetadataPublishedDefinition(t *testing.T) {
	profile, instance, definition := metadataFixture(t)
	calls := 0
	read := func(_ context.Context, _ buzzLookup, filters []nostr.Filter) ([]nostr.Event, error) {
		calls++
		if calls == 1 {
			if len(filters) != 2 || filters[0].Kinds[0] != 0 || filters[0].Authors[0] != profile.PubKey || filters[1].Kinds[0] != 30177 || filters[1].Tags["d"][0] != profile.PubKey {
				t.Fatal("metadata query was not limited to roster keys")
			}
			return []nostr.Event{profile, instance}, nil
		}
		if len(filters) != 1 || filters[0].Authors[0] != instance.PubKey || filters[0].Tags["d"][0] != "engineer" {
			t.Fatal("definition query was not tied to verified owner")
		}
		return []nostr.Event{definition}, nil
	}
	raw := []byte(`[{"pubkey":"` + profile.PubKey + `","name":"Ari","about":"An engineer"}]`)
	enriched := enrichBuzzRuntimeProfiles(context.Background(), testBuzzLookup(), raw, read)
	var value []struct{ Harness, Model, About string }
	if json.Unmarshal(enriched, &value) != nil || len(value) != 1 || value[0].Harness != "claude-code" || value[0].Model != "example-model" || value[0].About != "An engineer" || calls != 2 || strings.Contains(string(enriched), "must not reach client") {
		t.Fatalf("unexpected metadata projection %s", enriched)
	}
}

func TestBuzzRuntimeMetadataStandaloneModelAndOptionalFailure(t *testing.T) {
	profile, _, _ := metadataFixture(t)
	instance := metadataEvent(t, metadataOwnerKey, 30177, nostr.Tags{{"d", profile.PubKey}}, `{"model":"example-model","runtime":"do-not-trust"}`)
	calls := 0
	result := readBuzzRuntimeMetadata(context.Background(), testBuzzLookup(), []string{profile.PubKey}, func(context.Context, buzzLookup, []nostr.Filter) ([]nostr.Event, error) {
		calls++
		return []nostr.Event{profile, instance}, nil
	})
	if result[profile.PubKey].Model != "example-model" || result[profile.PubKey].Harness != "" || calls != 1 {
		t.Fatal("standalone model not read, or unpublished harness trusted")
	}
	for _, failure := range []string{"unsupported", "malformed", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			raw := []byte(`[{"pubkey":"` + profile.PubKey + `","name":"Ari","about":"Model inferred?","harness":"fake","model":"fake"}]`)
			value := enrichBuzzRuntimeProfiles(context.Background(), testBuzzLookup(), raw, func(ctx context.Context, _ buzzLookup, _ []nostr.Filter) ([]nostr.Event, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("optional query has no deadline")
				}
				return nil, errors.New(failure)
			})
			if !strings.Contains(string(value), `"name":"Ari"`) || strings.Contains(string(value), `"model"`) || strings.Contains(string(value), `"harness"`) {
				t.Fatal("optional failure changed roster or trusted kind-0 runtime claims")
			}
		})
	}
}

func TestBuzzRuntimeMetadataRejectsUnprovenAssociation(t *testing.T) {
	for _, scenario := range []string{"missing owner", "wrong owner signature", "wrong instance owner", "wrong definition owner", "invalid profile signature", "invalid instance signature", "invalid definition signature", "wrong conditions", "duplicate auth", "duplicate d", "new profile removes auth"} {
		t.Run(scenario, func(t *testing.T) {
			profile, instance, definition := metadataFixture(t)
			var newer *nostr.Event
			switch scenario {
			case "missing owner":
				profile.Tags = nostr.Tags{}
				_ = profile.Sign(metadataAgentKey)
			case "wrong owner signature":
				profile.Tags[0][3] = strings.Repeat("0", 128)
				_ = profile.Sign(metadataAgentKey)
			case "wrong instance owner":
				_ = instance.Sign(metadataOtherKey)
			case "wrong definition owner":
				_ = definition.Sign(metadataOtherKey)
			case "invalid profile signature":
				profile.Sig = strings.Repeat("0", 128)
			case "invalid instance signature":
				instance.Content += " "
			case "invalid definition signature":
				definition.Sig = strings.Repeat("0", 128)
			case "wrong conditions":
				profile.Tags = nostr.Tags{metadataAuth(t, profile.PubKey, "kind=9")}
				_ = profile.Sign(metadataAgentKey)
			case "duplicate auth":
				profile.Tags = append(profile.Tags, profile.Tags[0])
				_ = profile.Sign(metadataAgentKey)
			case "duplicate d":
				instance.Tags = append(instance.Tags, instance.Tags[0])
				_ = instance.Sign(metadataOwnerKey)
			case "new profile removes auth":
				copy := metadataEvent(t, metadataAgentKey, 0, nostr.Tags{}, `{}`)
				copy.CreatedAt++
				_ = copy.Sign(metadataAgentKey)
				newer = &copy
			}
			calls := 0
			result := readBuzzRuntimeMetadata(context.Background(), testBuzzLookup(), []string{profile.PubKey}, func(context.Context, buzzLookup, []nostr.Filter) ([]nostr.Event, error) {
				calls++
				if calls == 1 {
					events := []nostr.Event{profile, instance}
					if newer != nil {
						events = append(events, *newer)
					}
					return events, nil
				}
				return []nostr.Event{definition}, nil
			})
			if len(result) != 0 {
				t.Fatalf("unproven association was accepted: %+v", result)
			}
		})
	}
}

func TestBuzzMetadataConditions(t *testing.T) {
	event := nostr.Event{Kind: 0, CreatedAt: 100}
	for _, condition := range []string{"", "kind=0", "created_at<101&created_at>99", "kind=0&created_at<4294967295"} {
		if !buzzMetadataConditions(condition, event) {
			t.Fatalf("valid condition rejected: %s", condition)
		}
	}
	for _, condition := range []string{"kind=9", "kind=00", "kind=+0", "kind=65536", "created_at<100", "created_at>100", "created_at<4294967296", "kind=0&", "&kind=0", "kind=0&&kind=0", "kind=0 ", "invalid=0"} {
		if buzzMetadataConditions(condition, event) {
			t.Fatalf("invalid condition accepted: %s", condition)
		}
	}
}

func TestBuzzMetadataHTTPAuthorization(t *testing.T) {
	lookup := testBuzzLookup()
	lookup.PrivateKey, _ = nip19.EncodePrivateKey(metadataAgentKey)
	agent, _ := nostr.GetPublicKey(metadataAgentKey)
	tag, _ := json.Marshal(metadataAuth(t, agent, ""))
	lookup.AuthTag = string(tag)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/query" || r.Header.Get("X-Auth-Tag") != string(tag) || strings.Contains(string(body), metadataAgentKey) {
			t.Error("unexpected query or credential disclosure")
		}
		signed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(r.Header.Get("Authorization"), "Nostr "))
		var event nostr.Event
		if err != nil || json.Unmarshal(signed, &event) != nil || !validBuzzMetadataEvent(event) || event.Kind != 27235 || event.PubKey != agent {
			t.Error("invalid NIP-98 auth")
		}
		hash := sha256.Sum256(body)
		tags := map[string]string{}
		for _, tag := range event.Tags {
			if len(tag) == 2 {
				tags[tag[0]] = tag[1]
			}
		}
		if tags["u"] != lookup.RelayURL+"/query" || tags["method"] != "POST" || tags["payload"] != hex.EncodeToString(hash[:]) || tags["nonce"] == "" {
			t.Error("NIP-98 request binding is missing")
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	lookup.RelayURL = server.URL
	events, err := queryBuzzMetadataHTTP(context.Background(), lookup, []nostr.Filter{{Kinds: []int{0}}}, server.Client())
	if err != nil || len(events) != 0 {
		t.Fatalf("query failed: %v", err)
	}
}

func TestBuzzMetadataQueryBoundsAndCancellation(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `[] trailing`, strings.Repeat(" ", 2*1024*1024+1)} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		lookup := testBuzzLookup()
		lookup.RelayURL = server.URL
		_, err := queryBuzzMetadataHTTP(context.Background(), lookup, []nostr.Filter{{Kinds: []int{0}}}, server.Client())
		server.Close()
		if err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	enrichBuzzRuntimeProfiles(ctx, testBuzzLookup(), []byte(`[{"pubkey":"`+strings.Repeat("a", 64)+`"}]`), func(context.Context, buzzLookup, []nostr.Filter) ([]nostr.Event, error) {
		called = true
		return nil, nil
	})
	if called {
		t.Fatal("cancelled lookup started metadata request")
	}
	profile, instance, _ := metadataFixture(t)
	start := time.Now()
	enrichBuzzRuntimeProfiles(context.Background(), testBuzzLookup(), []byte(`[{"pubkey":"`+profile.PubKey+`"}]`), func(ctx context.Context, _ buzzLookup, filters []nostr.Filter) ([]nostr.Event, error) {
		if filters[0].Kinds[0] == 0 {
			return []nostr.Event{profile, instance}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if time.Since(start) > 3*time.Second {
		t.Fatal("optional metadata exceeded its shared deadline")
	}
}

func TestBuzzRosterExposesSeparateDescriptionAndRuntime(t *testing.T) {
	key := strings.Repeat("a", 64)
	member, err := lookupBuzzMembers(context.Background(), testBuzzLookup(), func(_ context.Context, _ buzzLookup, args ...string) ([]byte, error) {
		if args[0] == "channels" {
			return []byte(`[{"pubkey":"` + key + `","harness":"spoofed"}]`), nil
		}
		return json.Marshal([]map[string]string{{"pubkey": key, "about": strings.Repeat("x", 300), "harness": "claude-code\n", "model": "model\u202e one"}})
	})
	if err != nil || len(member) != 1 || len([]rune(member[0].Description)) != 280 || len([]rune(member[0].DefaultLabel)) != 80 || member[0].Harness != "claude-code" || member[0].Model != "model one" {
		t.Fatalf("unexpected roster projection: %+v, %v", member, err)
	}
}
