package main

import "testing"

func TestReplaceEmojiShortcodes(t *testing.T) {
	resetSeedEmojis()
	got := replaceEmojiShortcodes("bro :sob: :fire: https://example.com:8080/a:b 15:42:00 :unknown:")
	want := "bro 😭 🔥 https://example.com:8080/a:b 15:42:00 :unknown:"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestTrailingEmojiQueryAndSearch(t *testing.T) {
	resetSeedEmojis()
	prefix, query, ok := trailingEmojiQuery("hello :so")
	if !ok || prefix != "hello " || query != "so" {
		t.Fatalf("unexpected match: prefix=%q query=%q ok=%v", prefix, query, ok)
	}

	matches := searchEmojis(query, 8)
	if len(matches) == 0 || matches[0].Code != "sob" || matches[0].Emoji != "😭" {
		t.Fatalf("expected sob first for 'so', got %+v", matches)
	}

	for _, bad := range []string{"https://foo", "12:30", "hello :a", "hello :sob:"} {
		if _, _, ok := trailingEmojiQuery(bad); ok {
			t.Fatalf("expected no trailing emoji query for %q", bad)
		}
	}
}

func TestParseGemojiJSONAndTagSearch(t *testing.T) {
	defer resetSeedEmojis()

	sample := `[
		{"emoji":"🫠","description":"melting face","aliases":["melting_face"],"tags":["hot","heat"]},
		{"emoji":"🥹","description":"face holding back tears","aliases":["holding_back_tears"],"tags":["proud","happy"]}
	]`
	entries, byCode, err := parseGemojiJSON([]byte(sample))
	if err != nil {
		t.Fatalf("parseGemojiJSON failed: %v", err)
	}
	setEmojiCatalog(entries, byCode)

	if got := replaceEmojiShortcodes(":melting_face:"); got != "🫠" {
		t.Fatalf("expected 🫠, got %q", got)
	}
	if m := searchEmojis("happy", 5); len(m) == 0 || m[0].Emoji != "🥹" {
		t.Fatalf("expected tag match 🥹 for 'happy', got %+v", m)
	}
	if m := searchEmojis("tears", 5); len(m) == 0 || m[0].Emoji != "🥹" {
		t.Fatalf("expected description match 🥹 for 'tears', got %+v", m)
	}
}
