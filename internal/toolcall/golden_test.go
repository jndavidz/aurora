package toolcall

// golden 测试:固化真实/对抗性输入的解析快照,防行为漂移。
//
// 用法:
//	go test ./internal/toolcall/ -run TestGolden            // 常规对比
//	go test ./internal/toolcall/ -run TestGolden -update    // 重新生成 .golden
//
// 样本约定:testdata/<name>.input.txt(模型原始输出,可含多字节 UTF-8)
//           testdata/<name>.golden(解析快照 JSON:text + calls)
//
// 解析方式统一为"流式逐块喂入"(块大小 5 字节,刻意制造尴尬切点,
// 覆盖标签被截断的路径)—— 与生产路径(chat_handler 流式转发)一致。

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aurora/typings/official"
)

var updateGolden = flag.Bool("update", false, "重写 testdata/*.golden 快照")

type goldenSnapshot struct {
	Text string          `json:"text"`
	Calls []goldenCall   `json:"calls"`
}

type goldenCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	IDPrefix  string `json:"idPrefix"` // call_ 前缀存在性(不固化随机后缀)
}

// runGolden 对单个样本执行"流式逐块解析"并输出快照字符串。
func runGolden(t *testing.T, input string, tags TagSet, tools []official.Tool) string {
	t.Helper()
	var p *Parser
	switch {
	case tools != nil:
		p = NewParserWithTagsAndTools(tags, tools)
	case tags.StartTag != "":
		p = NewParserWithTags(tags)
	default:
		p = NewParser()
	}

	var text strings.Builder
	var calls []goldenCall
	const chunkSize = 5 // 字节;UTF-8 多字节字符会被切半 —— 正是生产中的真实情况
	for i := 0; i < len(input); i += chunkSize {
		end := i + chunkSize
		if end > len(input) {
			end = len(input)
		}
		td, cs := p.Feed(input[i:end])
		text.WriteString(td)
		for _, c := range cs {
			calls = append(calls, goldenCall{
				Name:      c.Function.Name,
				Arguments: c.Function.Arguments,
				IDPrefix:  idPrefix(c.ID),
			})
		}
	}
	td, cs := p.Flush()
	text.WriteString(td)
	for _, c := range cs {
		calls = append(calls, goldenCall{
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
			IDPrefix:  idPrefix(c.ID),
		})
	}

	snap := goldenSnapshot{Text: text.String(), Calls: calls}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func idPrefix(id string) string {
	if strings.HasPrefix(id, "call_") {
		return "call_"
	}
	return id
}

// TestGolden 遍历 testdata/*.input.txt,与 *.golden 快照对比。
func TestGolden(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.input.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("testdata/ 下没有样本 —— 治理事故")
	}
	for _, input := range paths {
		name := strings.TrimSuffix(filepath.Base(input), ".input.txt")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			in := string(raw)

			// 样本首行可指定标签集:#!deepseek → DeepSeek 特殊标签;默认 ChatGPT 标签
			var tags TagSet
			if strings.HasPrefix(in, "#!deepseek\n") {
				tags = TagSet{StartTag: "<|tool\u2581calls\u2581begin|>", EndTag: "<|tool\u2581calls\u2581end|>"}
				in = strings.TrimPrefix(in, "#!deepseek\n")
			}

			got := runGolden(t, in, tags, nil)

			golden := strings.TrimSuffix(input, ".input.txt") + ".golden"
			if *updateGolden {
				if err := os.WriteFile(golden, []byte(got+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("缺 .golden(跑 -update 生成): %v", err)
			}
			if got+"\n" != string(want) {
				t.Errorf("快照不一致:\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
			}
		})
	}
}
