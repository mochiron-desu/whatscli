package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/adrg/xdg"
	"github.com/normen/whatscli/config"
)

const (
	emojiHandbookURL = "https://raw.githubusercontent.com/github/gemoji/master/db/emoji.json"
	emojiCacheMaxAge = 7 * 24 * time.Hour
)

type emojiEntry struct {
	Code        string
	Emoji       string
	Description string
	Tags        []string
}

type gemojiRecord struct {
	Emoji       string   `json:"emoji"`
	Description string   `json:"description"`
	Aliases     []string `json:"aliases"`
	Tags        []string `json:"tags"`
}

var seedEmojiEntries = []emojiEntry{
	{Code: "sob", Emoji: "😭", Description: "loudly crying face", Tags: []string{"sad", "cry", "tears"}},
	{Code: "joy", Emoji: "😂", Description: "face with tears of joy", Tags: []string{"laugh", "happy"}},
	{Code: "rofl", Emoji: "🤣", Description: "rolling on the floor laughing", Tags: []string{"lol", "lmao"}},
	{Code: "cry", Emoji: "😢", Description: "crying face", Tags: []string{"sad", "tear"}},
	{Code: "fire", Emoji: "🔥", Description: "fire", Tags: []string{"lit", "hot"}},
	{Code: "skull", Emoji: "💀", Description: "skull", Tags: []string{"dead"}},
	{Code: "100", Emoji: "💯", Description: "hundred points", Tags: []string{"score", "perfect"}},
	{Code: "heart", Emoji: "❤️", Description: "red heart", Tags: []string{"love"}},
	{Code: "thumbsup", Emoji: "👍", Description: "thumbs up", Tags: []string{"approve", "ok"}},
	{Code: "+1", Emoji: "👍", Description: "thumbs up"},
	{Code: "pray", Emoji: "🙏", Description: "folded hands", Tags: []string{"please", "thanks"}},
	{Code: "sparkles", Emoji: "✨", Description: "sparkles", Tags: []string{"shiny"}},
	{Code: "eyes", Emoji: "👀", Description: "eyes", Tags: []string{"look", "watch"}},
	{Code: "thinking", Emoji: "🤔", Description: "thinking face", Tags: []string{"hmm"}},
	{Code: "sweat_smile", Emoji: "😅", Description: "grinning face with sweat", Tags: []string{"hot", "nervous"}},
}

var (
	emojiMu               sync.RWMutex
	emojiEntries          []emojiEntry
	emojiByCode           map[string]string
	emojiShortcodePattern = regexp.MustCompile(`:([a-zA-Z0-9_+-]+):`)
	trailingEmojiPattern  = regexp.MustCompile(`(?:^|\s):([a-zA-Z0-9_+-]{2,})$`)
)

func init() {
	resetSeedEmojis()
}

func resetSeedEmojis() {
	byCode := make(map[string]string, len(seedEmojiEntries))
	entries := make([]emojiEntry, len(seedEmojiEntries))
	copy(entries, seedEmojiEntries)
	for _, entry := range entries {
		byCode[entry.Code] = entry.Emoji
	}
	emojiMu.Lock()
	emojiEntries = entries
	emojiByCode = byCode
	emojiMu.Unlock()
}

func parseGemojiJSON(data []byte) ([]emojiEntry, map[string]string, error) {
	var records []gemojiRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, nil, err
	}
	entries := make([]emojiEntry, 0, len(records))
	byCode := make(map[string]string, len(records))
	for _, rec := range records {
		if rec.Emoji == "" {
			continue
		}
		desc := strings.ToLower(strings.TrimSpace(rec.Description))
		tags := make([]string, 0, len(rec.Tags))
		for _, t := range rec.Tags {
			if lt := strings.ToLower(strings.TrimSpace(t)); lt != "" {
				tags = append(tags, lt)
			}
		}
		for _, alias := range rec.Aliases {
			code := strings.ToLower(strings.TrimSpace(alias))
			if code == "" {
				continue
			}
			byCode[code] = rec.Emoji
			entries = append(entries, emojiEntry{
				Code:        code,
				Emoji:       rec.Emoji,
				Description: desc,
				Tags:        tags,
			})
		}
	}
	if len(entries) == 0 {
		return nil, nil, errors.New("no emoji entries found in handbook")
	}
	return entries, byCode, nil
}

func setEmojiCatalog(entries []emojiEntry, byCode map[string]string) {
	emojiMu.Lock()
	emojiEntries = entries
	emojiByCode = byCode
	emojiMu.Unlock()
}

func emojiCachePath() string {
	if p, err := xdg.CacheFile("whatscli/emojis.json"); err == nil {
		return p
	}
	return config.GetSessionFilePath() + ".emojis.json"
}

func loadEmojiHandbook(forceRefresh bool) (int, error) {
	cachePath := emojiCachePath()
	if !forceRefresh {
		if data, err := os.ReadFile(cachePath); err == nil {
			if entries, byCode, pErr := parseGemojiJSON(data); pErr == nil {
				setEmojiCatalog(entries, byCode)
				if info, sErr := os.Stat(cachePath); sErr == nil && time.Since(info.ModTime()) < emojiCacheMaxAge {
					return len(entries), nil
				}
			}
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(emojiHandbookURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, errors.New("emoji handbook HTTP " + resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return 0, err
	}
	entries, byCode, err := parseGemojiJSON(body)
	if err != nil {
		return 0, err
	}
	setEmojiCatalog(entries, byCode)

	if mkErr := os.MkdirAll(filepath.Dir(cachePath), 0o755); mkErr == nil {
		_ = os.WriteFile(cachePath, body, 0o644)
	}
	return len(entries), nil
}

func initEmojiCatalog() {
	_, _ = loadEmojiHandbook(false)
}

func replaceEmojiShortcodes(text string) string {
	emojiMu.RLock()
	defer emojiMu.RUnlock()
	return emojiShortcodePattern.ReplaceAllStringFunc(text, func(match string) string {
		code := strings.ToLower(match[1 : len(match)-1])
		if emoji, ok := emojiByCode[code]; ok {
			return emoji
		}
		return match
	})
}

func trailingEmojiQuery(text string) (prefix, query string, ok bool) {
	loc := trailingEmojiPattern.FindStringSubmatchIndex(text)
	if loc == nil {
		return "", "", false
	}
	queryStart := loc[2]
	queryEnd := loc[3]
	return text[:queryStart-1], strings.ToLower(text[queryStart:queryEnd]), true
}

func searchEmojis(query string, limit int) []emojiEntry {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || limit <= 0 {
		return nil
	}

	emojiMu.RLock()
	entries := emojiEntries
	emojiMu.RUnlock()

	var exact, prefix, contains, tagMatch, descMatch []emojiEntry
	seenCode := make(map[string]bool)

	for _, entry := range entries {
		if seenCode[entry.Code] {
			continue
		}
		seenCode[entry.Code] = true
		switch {
		case entry.Code == q:
			exact = append(exact, entry)
		case strings.HasPrefix(entry.Code, q):
			prefix = append(prefix, entry)
		case strings.Contains(entry.Code, q):
			contains = append(contains, entry)
		default:
			matchedTag := false
			for _, tag := range entry.Tags {
				if tag == q || strings.HasPrefix(tag, q) {
					matchedTag = true
					break
				}
			}
			if matchedTag {
				tagMatch = append(tagMatch, entry)
			} else if entry.Description != "" && strings.Contains(entry.Description, q) {
				descMatch = append(descMatch, entry)
			}
		}
	}

	out := make([]emojiEntry, 0, limit)
	for _, group := range [][]emojiEntry{exact, prefix, contains, tagMatch, descMatch} {
		for _, entry := range group {
			out = append(out, entry)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}
