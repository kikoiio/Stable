package sessionlog

import (
	"os"
	"strings"
	"testing"
)

func TestSearchMatchesTitleAndEventText(t *testing.T) {
	root := t.TempDir()
	alpha, err := Create(root, "修复登录超时")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, alpha.ID, EventMessage, Message{Role: "user", Text: "排查一下数据库连接"}); err != nil {
		t.Fatal(err)
	}
	beta, err := Create(root, "别的会话")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, beta.ID, EventMessage, Message{Role: "user", Text: "登录页样式调整"}); err != nil {
		t.Fatal(err)
	}

	result, err := Search(root, "登录", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Corrupt) != 0 {
		t.Fatalf("unexpected corrupt: %+v", result.Corrupt)
	}
	if len(result.Hits) == 0 {
		t.Fatal("no hits for 登录")
	}
	fields := map[string]bool{}
	for _, hit := range result.Hits {
		fields[hit.Field] = true
		if hit.Session.ID != alpha.ID && hit.Session.ID != beta.ID {
			t.Fatalf("hit from unknown session: %+v", hit)
		}
	}
	if !fields["title"] || !fields["event"] {
		t.Fatalf("expected title and event hits, got %+v", result.Hits)
	}

	if _, err = Search(root, "不存在的词", 0); err != nil {
		t.Fatal(err)
	}
}

func TestSearchSnippetIsBounded(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "long")
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("前", 200)
	text := pad + "关键字" + strings.Repeat("后", 200)
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: text}); err != nil {
		t.Fatal(err)
	}
	result, err := Search(root, "关键字", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %+v", result.Hits)
	}
	snippet := []rune(result.Hits[0].Snippet)
	if len(snippet) > 2*SearchSnippetRadius+3+2 {
		t.Fatalf("snippet length = %d runes", len(snippet))
	}
	if !strings.HasPrefix(result.Hits[0].Snippet, "…") || !strings.HasSuffix(result.Hits[0].Snippet, "…") {
		t.Fatalf("snippet missing ellipsis: %q", result.Hits[0].Snippet)
	}
}

func TestSearchReportsCorruptSession(t *testing.T) {
	root := t.TempDir()
	good, err := Create(root, "good")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, good.ID, EventMessage, Message{Role: "user", Text: "可搜索内容"}); err != nil {
		t.Fatal(err)
	}
	bad, err := Create(root, "bad")
	if err != nil {
		t.Fatal(err)
	}
	path, err := SessionPath(root, bad.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("{broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Search(root, "可搜索", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Session.ID != good.ID {
		t.Fatalf("hits = %+v", result.Hits)
	}
	if len(result.Corrupt) != 1 || result.Corrupt[0].SessionID != bad.ID {
		t.Fatalf("corrupt = %+v", result.Corrupt)
	}
}

func TestSearchLimitAndEmptyQuery(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "limited")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "目标词 消息"}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Search(root, "目标词", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(result.Hits))
	}
	empty, err := Search(root, "  ", 0)
	if err != nil || len(empty.Hits) != 0 {
		t.Fatalf("empty query = %+v, %v", empty, err)
	}
}
