package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func companionProjectDate(value string) string {
	stamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || stamp.IsZero() {
		return ""
	}
	return stamp.UTC().Format(time.RFC3339Nano)
}

func companionProjectMetadata(project companionProject) companionProject {
	root, err := filepath.EvalSymlinks(project.path)
	if err != nil || root != project.path {
		return project
	}
	manifest, err := readChatProjectFile(project.path, "glowbom.json", 1<<20)
	if err != nil {
		return project
	}
	var value struct {
		CreatedAt string `json:"createdAt"`
	}
	if json.Unmarshal([]byte(manifest), &value) == nil {
		project.CreatedAt = companionProjectDate(value.CreatedAt)
	}
	return project
}

func (s *companionSession) projectCatalog() []companionProject {
	projects := []companionProject{}
	s.mu.Lock()
	for _, project := range s.projects {
		projects = append(projects, project)
	}
	s.mu.Unlock()
	// Refresh metadata only for folders already shared by this pairing. Listing
	// must not discover or grant access to any additional Desktop project.
	for i, project := range projects {
		projects[i] = companionProjectMetadata(project)
	}
	sort.Slice(projects, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339Nano, projects[i].CreatedAt)
		right, _ := time.Parse(time.RFC3339Nano, projects[j].CreatedAt)
		if !left.Equal(right) {
			return left.After(right)
		}
		if projects[i].Name != projects[j].Name {
			return projects[i].Name < projects[j].Name
		}
		return projects[i].ID < projects[j].ID
	})
	return projects
}

// Paging is the only query accepted for the companion audio catalog. Other
// companion routes keep their existing restrictions on query parameters.
func companionAudioPageRequest(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != companionPrefix+"/audio" || len(r.URL.RawQuery) > 64 {
		return false
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) == 0 || len(query) > 2 {
		return false
	}
	for key, values := range query {
		if (key != "offset" && key != "limit") || len(values) != 1 || values[0] == "" || strings.IndexFunc(values[0], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return false
		}
		value, err := strconv.Atoi(values[0])
		if err != nil || value < 0 || (key == "limit" && (value == 0 || value > 48)) {
			return false
		}
	}
	return true
}

func (s *companionSession) routeAudioLibrary(w http.ResponseWriter, r *http.Request, path string, parts []string) bool {
	if path == "/audio" && r.Method == http.MethodGet {
		query := r.URL.Query()
		paging := url.Values{}
		for _, key := range []string{"offset", "limit"} {
			if value := query.Get(key); value != "" {
				paging.Set(key, value)
			}
		}
		target := "/studio/audio"
		if len(paging) > 0 {
			target += "?" + paging.Encode()
		}
		s.api.ServeHTTP(w, s.internalRequest(r.Context(), http.MethodGet, target, nil))
		return true
	}
	if len(parts) != 3 || parts[0] != "audio" || parts[2] != "content" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	id := normalizedStudioUUID(parts[1])
	if id == "" {
		http.NotFound(w, r)
		return true
	}
	request := s.internalRequest(r.Context(), r.Method, "/studio/audio/content?id="+url.QueryEscape(id), nil)
	for _, header := range []string{"Range", "If-Range"} {
		if value := r.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	s.api.ServeHTTP(w, request)
	return true
}
