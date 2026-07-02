// diff_coverage 计算「改动行覆盖率」：本次 diff 中的可执行行有多少被
// coverprofile（unit + integration 任一）覆盖。这是 deep-review 的核心门禁——
// 全仓库覆盖率会被历史代码稀释，只有改动行覆盖率能回答"这次改的东西测了没"。
//
// 用法（在仓库根目录）：
//
//	go run .claude/skills/deep-review/scripts/diff_coverage.go \
//	    -profiles tmp/deep-review/unit.out,tmp/deep-review/integration.out \
//	    -threshold 80
//
// 位于 .claude/ 下，go 工具链遍历 ./... 时会忽略本文件，不污染主模块。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type block struct {
	start, end int
	count      int
}

func main() {
	baseFlag := flag.String("base", "", "git 基线（默认 merge-base main HEAD）")
	profilesFlag := flag.String("profiles", "", "逗号分隔的 coverprofile 路径")
	threshold := flag.Float64("threshold", 80, "改动行覆盖率阈值（百分比）")
	exemptFlag := flag.String("exempt", "cmd/", "逗号分隔的豁免路径前缀（装配代码）")
	flag.Parse()

	module := modulePath()

	base := *baseFlag
	if base == "" {
		base = strings.TrimSpace(gitOut("merge-base", "main", "HEAD"))
	}

	changed, wholeFiles := changedLines(base)
	if len(changed) == 0 && len(wholeFiles) == 0 {
		fmt.Println("没有 .go 非测试文件改动，改动行覆盖率检查跳过。")
		return
	}

	blocks := map[string][]block{}
	for _, p := range strings.Split(*profilesFlag, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if err := parseProfile(p, module, blocks); err != nil {
			fmt.Fprintf(os.Stderr, "警告：无法读取 profile %s：%v（该 profile 的覆盖不计入）\n", p, err)
		}
	}
	if len(blocks) == 0 {
		fmt.Fprintln(os.Stderr, "错误：没有任何可用的 coverprofile，无法计算覆盖率")
		os.Exit(2)
	}

	exempts := strings.Split(*exemptFlag, ",")
	isExempt := func(f string) bool {
		for _, e := range exempts {
			if e != "" && strings.HasPrefix(f, strings.TrimSpace(e)) {
				return true
			}
		}
		return false
	}

	// 未跟踪的新文件没有 diff 行号，视为整个文件都是改动行。
	for f := range wholeFiles {
		set := map[int]bool{}
		for _, b := range blocks[f] {
			for l := b.start; l <= b.end; l++ {
				set[l] = true
			}
		}
		changed[f] = set
	}

	files := make([]string, 0, len(changed))
	for f := range changed {
		files = append(files, f)
	}
	sort.Strings(files)

	var totalExec, totalCovered int
	fmt.Printf("改动行覆盖率（基线 %s，阈值 %.0f%%）\n\n", short(base), *threshold)
	fmt.Printf("%-60s %8s %8s %7s\n", "文件", "可执行行", "已覆盖", "覆盖率")
	for _, f := range files {
		execLines, covered, uncovered := coverFile(changed[f], blocks[f])
		tag := ""
		if isExempt(f) {
			tag = "  [豁免，不计入门禁]"
		} else {
			totalExec += execLines
			totalCovered += covered
		}
		pct := "-"
		if execLines > 0 {
			pct = fmt.Sprintf("%.1f%%", 100*float64(covered)/float64(execLines))
		}
		fmt.Printf("%-60s %8d %8d %7s%s\n", f, execLines, covered, pct, tag)
		if len(uncovered) > 0 && !isExempt(f) {
			fmt.Printf("    未覆盖行: %s\n", ranges(uncovered))
		}
	}

	fmt.Println()
	if totalExec == 0 {
		fmt.Println("改动中没有可执行语句（纯类型/注释/文档改动），视为通过。")
		return
	}
	pct := 100 * float64(totalCovered) / float64(totalExec)
	fmt.Printf("总计：%d/%d 行覆盖 = %.1f%%\n", totalCovered, totalExec, pct)
	if pct < *threshold {
		fmt.Printf("结果：FAIL（低于阈值 %.0f%%）\n", *threshold)
		os.Exit(1)
	}
	fmt.Println("结果：PASS")
}

func modulePath() string {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		fatal("读取 go.mod 失败: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	fatal("go.mod 中找不到 module 声明")
	return ""
}

// changedLines 返回已跟踪文件的改动行号集合，以及未跟踪的新 .go 文件集合。
func changedLines(base string) (map[string]map[int]bool, map[string]bool) {
	changed := map[string]map[int]bool{}
	hunk := regexp.MustCompile(`^@@ .*\+(\d+)(?:,(\d+))? @@`)
	var cur string
	out := gitOut("diff", "-U0", base, "--", "*.go")
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "+++ b/") {
			cur = strings.TrimPrefix(line, "+++ b/")
			// .claude/ 下是 skill 自身的脚本，go 工具链不编译它们
			if strings.HasSuffix(cur, "_test.go") || !strings.HasSuffix(cur, ".go") || strings.HasPrefix(cur, ".claude/") {
				cur = ""
			}
			continue
		}
		if strings.HasPrefix(line, "+++ ") { // /dev/null 等
			cur = ""
			continue
		}
		if cur == "" {
			continue
		}
		if m := hunk.FindStringSubmatch(line); m != nil {
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			if changed[cur] == nil {
				changed[cur] = map[int]bool{}
			}
			for i := 0; i < n; i++ {
				changed[cur][start+i] = true
			}
		}
	}

	whole := map[string]bool{}
	for _, f := range strings.Split(gitOut("ls-files", "--others", "--exclude-standard", "--", "*.go"), "\n") {
		f = strings.TrimSpace(f)
		if f != "" && strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") && !strings.HasPrefix(f, ".claude/") {
			whole[f] = true
		}
	}
	return changed, whole
}

func parseProfile(path, module string, blocks map[string][]block) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		// 格式: module/path/file.go:startL.startC,endL.endC numStmt count
		colon := strings.LastIndex(line, ".go:")
		if colon < 0 {
			continue
		}
		file := strings.TrimPrefix(line[:colon+3], module+"/")
		rest := line[colon+4:]
		var sl, sc2, el, ec, stmts, count int
		if _, err := fmt.Sscanf(rest, "%d.%d,%d.%d %d %d", &sl, &sc2, &el, &ec, &stmts, &count); err != nil {
			continue
		}
		blocks[file] = append(blocks[file], block{start: sl, end: el, count: count})
	}
	return sc.Err()
}

// coverFile 求改动行与 profile 块的交集：不在任何块内的行是不可执行行
// （声明/注释/空行），不进分母。
func coverFile(lines map[int]bool, bs []block) (execN, covered int, uncovered []int) {
	for l := range lines {
		inBlock, hit := false, false
		for _, b := range bs {
			if l >= b.start && l <= b.end {
				inBlock = true
				if b.count > 0 {
					hit = true
					break
				}
			}
		}
		if inBlock {
			execN++
			if hit {
				covered++
			} else {
				uncovered = append(uncovered, l)
			}
		}
	}
	sort.Ints(uncovered)
	return
}

func ranges(lines []int) string {
	var parts []string
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(lines[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", lines[i], lines[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

func gitOut(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		fatal("git %s 失败: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func short(ref string) string {
	if len(ref) > 8 {
		return ref[:8]
	}
	return ref
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误："+format+"\n", a...)
	os.Exit(2)
}
