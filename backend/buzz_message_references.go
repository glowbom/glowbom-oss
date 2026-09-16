package main

import (
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// buzzMessageReferences reads identity and immediate-parent links from an already
// verified channel event. Buzz uses explicit p tags for mentions and marked e
// tags for replies. A bare e tag or a root marker alone is not a reply.
// See buzz-core/src/nip10.rs and buzz-sdk/src/builders.rs at revision 3c7f288.
func buzzMessageReferences(tags nostr.Tags) ([]string, string) {
	mentions := make([]string, 0)
	seen := make(map[string]bool)
	replyTo := ""
	for _, tag := range tags {
		if len(tag) < 2 || !buzzHexKey.MatchString(tag[1]) {
			continue
		}
		switch tag[0] {
		case "p":
			pubkey := strings.ToLower(tag[1])
			if !seen[pubkey] {
				seen[pubkey] = true
				mentions = append(mentions, pubkey)
			}
		case "e":
			if len(tag) >= 4 && tag[3] == "reply" {
				// Match Buzz's last-valid-marker rule for repeated reply tags.
				replyTo = strings.ToLower(tag[1])
			}
		}
	}
	return mentions, replyTo
}
