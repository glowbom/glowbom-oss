// Created by Codex for Glowbom OSS.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

// Verified against block/buzz 3c7f288c60d67df78577b237e27c3dfc8831aaa1:
// agent_events.rs, persona_events.rs, buzz-cli/src/client.rs, and NIP-OA.
// Runtime is published on a linked kind-30175 definition. Standalone kind-30177
// records publish only the model. Local harness overrides are not on the relay.
type buzzRuntimeMetadata struct {
	Harness string
	Model   string
}

type buzzMetadataQuery func(context.Context, buzzLookup, []nostr.Filter) ([]nostr.Event, error)

var buzzPersonaSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Metadata is optional and shares a short deadline across both bridge reads.
// Never infer a harness or model by splitting the human-written description.
func enrichBuzzRuntimeProfiles(ctx context.Context, lookup buzzLookup, raw []byte, query buzzMetadataQuery) []byte {
	var profiles []map[string]json.RawMessage
	if json.Unmarshal(raw, &profiles) != nil || len(profiles) == 0 || len(profiles) > 200 {
		return raw
	}
	pubkeys := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		// Only the verified event projection below may supply these fields.
		delete(profile, "harness")
		delete(profile, "model")
		var key string
		if json.Unmarshal(profile["pubkey"], &key) == nil && buzzHexKey.MatchString(key) {
			pubkeys = append(pubkeys, strings.ToLower(key))
		}
	}
	metadata := map[string]buzzRuntimeMetadata{}
	if len(pubkeys) > 0 && ctx.Err() == nil {
		// Keep a little time for the required roster response if its deadline is near.
		budget := 2 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			budget = min(budget, time.Until(deadline)/2)
		}
		if budget >= 100*time.Millisecond {
			optional, cancel := context.WithTimeout(ctx, budget)
			metadata = readBuzzRuntimeMetadata(optional, lookup, pubkeys, query)
			cancel()
		}
	}
	for _, profile := range profiles {
		var key string
		_ = json.Unmarshal(profile["pubkey"], &key)
		if value, ok := metadata[strings.ToLower(key)]; ok {
			profile["harness"], _ = json.Marshal(value.Harness)
			profile["model"], _ = json.Marshal(value.Model)
		}
	}
	encoded, err := json.Marshal(profiles)
	if err != nil {
		return raw
	}
	return encoded
}

func readBuzzRuntimeMetadata(ctx context.Context, lookup buzzLookup, pubkeys []string, query buzzMetadataQuery) map[string]buzzRuntimeMetadata {
	result := map[string]buzzRuntimeMetadata{}
	events, err := query(ctx, lookup, []nostr.Filter{
		{Kinds: []int{0}, Authors: pubkeys, Limit: len(pubkeys)},
		{Kinds: []int{30177}, Tags: nostr.TagMap{"d": pubkeys}, Limit: len(pubkeys) * 2},
	})
	if err != nil {
		return result
	}
	wanted := map[string]bool{}
	for _, key := range pubkeys {
		wanted[key] = true
	}
	profiles := map[string]nostr.Event{}
	instances := map[string]nostr.Event{}
	for _, event := range events {
		if (event.Kind != 0 && event.Kind != 30177) || !validBuzzMetadataEvent(event) {
			continue
		}
		if event.Kind == 0 && wanted[event.PubKey] {
			buzzKeepLatest(profiles, event.PubKey, event)
		} else if event.Kind == 30177 {
			if key := buzzMetadataDTag(event); wanted[key] {
				buzzKeepLatest(instances, event.PubKey+":"+key, event)
			}
		}
	}
	type definitionRef struct{ owner, slug string }
	linked := map[string]definitionRef{}
	owners, slugs := []string{}, []string{}
	seenOwners, seenSlugs := map[string]bool{}, map[string]bool{}
	for key, profile := range profiles {
		owner := buzzMetadataOwner(profile)
		if owner == "" {
			continue
		}
		instance, ok := instances[owner+":"+key]
		if !ok {
			continue
		}
		var content struct {
			PersonaID string `json:"persona_id"`
			Model     string `json:"model"`
		}
		if json.Unmarshal([]byte(instance.Content), &content) != nil {
			continue
		}
		if content.PersonaID == "" {
			result[key] = buzzRuntimeMetadata{Model: buzzPublicText(content.Model, 80)}
			continue
		}
		if !buzzPersonaSlug.MatchString(content.PersonaID) {
			continue
		}
		linked[key] = definitionRef{owner, content.PersonaID}
		if !seenOwners[owner] {
			seenOwners[owner] = true
			owners = append(owners, owner)
		}
		if !seenSlugs[content.PersonaID] {
			seenSlugs[content.PersonaID] = true
			slugs = append(slugs, content.PersonaID)
		}
	}
	if len(linked) == 0 || ctx.Err() != nil {
		return result
	}
	events, err = query(ctx, lookup, []nostr.Filter{{Kinds: []int{30175}, Authors: owners, Tags: nostr.TagMap{"d": slugs}, Limit: min(1000, len(linked)*2)}})
	if err != nil {
		return result
	}
	definitions := map[string]nostr.Event{}
	for _, event := range events {
		if event.Kind != 30175 || !seenOwners[event.PubKey] || !validBuzzMetadataEvent(event) {
			continue
		}
		if slug := buzzMetadataDTag(event); seenSlugs[slug] {
			buzzKeepLatest(definitions, event.PubKey+":"+slug, event)
		}
	}
	for key, ref := range linked {
		definition, ok := definitions[ref.owner+":"+ref.slug]
		if !ok {
			continue
		}
		var content struct {
			Runtime string `json:"runtime"`
			Model   string `json:"model"`
		}
		if json.Unmarshal([]byte(definition.Content), &content) == nil {
			result[key] = buzzRuntimeMetadata{Harness: buzzPublicText(content.Runtime, 80), Model: buzzPublicText(content.Model, 80)}
		}
	}
	return result
}

func buzzKeepLatest(events map[string]nostr.Event, key string, event nostr.Event) {
	previous, ok := events[key]
	if !ok || event.CreatedAt > previous.CreatedAt || (event.CreatedAt == previous.CreatedAt && event.ID < previous.ID) {
		events[key] = event
	}
}

func validBuzzMetadataEvent(event nostr.Event) bool {
	if len(event.Content) > 256*1024 || event.CreatedAt < 0 || event.CreatedAt > nostr.Now()+60 || !event.CheckID() {
		return false
	}
	valid, err := event.CheckSignature()
	return err == nil && valid
}

func buzzMetadataDTag(event nostr.Event) string {
	value, count := "", 0
	for _, tag := range event.Tags {
		if len(tag) > 0 && tag[0] == "d" {
			count++
			if len(tag) == 2 {
				value = tag[1]
			}
		}
	}
	if count != 1 {
		return ""
	}
	return value
}

// NIP-OA provenance binds the agent's signed profile to its actual owner.
// A foreign owner naming a roster key in a d tag is not sufficient evidence.
func buzzMetadataOwner(event nostr.Event) string {
	var auth nostr.Tag
	for _, tag := range event.Tags {
		if len(tag) > 0 && tag[0] == "auth" {
			if auth != nil {
				return ""
			}
			auth = tag
		}
	}
	if len(auth) != 4 || len(auth[1]) != 64 || len(auth[3]) != 128 || auth[1] != strings.ToLower(auth[1]) || auth[3] != strings.ToLower(auth[3]) || auth[1] == event.PubKey || !buzzMetadataConditions(auth[2], event) {
		return ""
	}
	keyBytes, keyErr := hex.DecodeString(auth[1])
	sigBytes, sigErr := hex.DecodeString(auth[3])
	if keyErr != nil || sigErr != nil {
		return ""
	}
	key, keyErr := schnorr.ParsePubKey(keyBytes)
	sig, sigErr := schnorr.ParseSignature(sigBytes)
	if keyErr != nil || sigErr != nil {
		return ""
	}
	hash := sha256.Sum256([]byte("nostr:agent-auth:" + event.PubKey + ":" + auth[2]))
	if !sig.Verify(hash[:], key) {
		return ""
	}
	return auth[1]
}

func buzzMetadataConditions(conditions string, event nostr.Event) bool {
	if conditions == "" {
		return true
	}
	if len(conditions) > 4096 {
		return false
	}
	for _, clause := range strings.Split(conditions, "&") {
		prefix, limit := "", uint64(4294967295)
		for _, candidate := range []string{"kind=", "created_at<", "created_at>"} {
			if strings.HasPrefix(clause, candidate) {
				prefix = candidate
				break
			}
		}
		if prefix == "" {
			return false
		}
		if prefix == "kind=" {
			limit = 65535
		}
		decimal := strings.TrimPrefix(clause, prefix)
		number, err := strconv.ParseUint(decimal, 10, 64)
		if err != nil || number > limit || strconv.FormatUint(number, 10) != decimal {
			return false
		}
		if (prefix == "kind=" && uint64(event.Kind) != number) || (prefix == "created_at<" && uint64(event.CreatedAt) >= number) || (prefix == "created_at>" && uint64(event.CreatedAt) <= number) {
			return false
		}
	}
	return true
}

func queryBuzzMetadata(ctx context.Context, lookup buzzLookup, filters []nostr.Filter) ([]nostr.Event, error) {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return queryBuzzMetadataHTTP(ctx, lookup, filters, client)
}

// This is Buzz CLI's read-only HTTP query bridge, not the event publishing API.
// Keys sign a NIP-98 authorization envelope and never appear in the request.
func queryBuzzMetadataHTTP(ctx context.Context, lookup buzzLookup, filters []nostr.Filter, client *http.Client) ([]nostr.Event, error) {
	failure := errors.New("Buzz runtime metadata unavailable")
	if !validBuzzLookup(lookup) || len(filters) == 0 || len(filters) > 2 {
		return nil, failure
	}
	key := lookup.PrivateKey
	if strings.HasPrefix(key, "nsec") {
		prefix, decoded, err := nip19.Decode(key)
		if err != nil || prefix != "nsec" {
			return nil, failure
		}
		key = decoded.(string)
	}
	body, err := json.Marshal(filters)
	if err != nil || len(body) > 64*1024 {
		return nil, failure
	}
	endpoint := strings.TrimRight(lookup.RelayURL, "/") + "/query"
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return nil, failure
	}
	digest := sha256.Sum256(body)
	auth := nostr.Event{Kind: 27235, CreatedAt: nostr.Now(), Content: "", Tags: nostr.Tags{
		{"u", endpoint}, {"method", "POST"}, {"payload", hex.EncodeToString(digest[:])}, {"nonce", hex.EncodeToString(nonce)},
	}}
	if auth.Sign(key) != nil {
		return nil, failure
	}
	signed, err := json.Marshal(auth)
	if err != nil {
		return nil, failure
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, failure
	}
	request.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(signed))
	request.Header.Set("Content-Type", "application/json")
	if lookup.AuthTag != "" {
		request.Header.Set("X-Auth-Tag", lookup.AuthTag)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, failure
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, failure
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	var events []nostr.Event
	if err != nil || len(raw) > 2*1024*1024 || json.Unmarshal(raw, &events) != nil || events == nil || len(events) > 1000 {
		return nil, failure
	}
	return events, nil
}
