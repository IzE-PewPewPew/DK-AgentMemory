// Package compose turns a rough description of a task into a finished prompt.
//
// The service already holds, per project, the lessons and decisions distilled
// from months of real sessions. That is the whole reason this exists: a generic
// prompt generator can tell a model to "write secure code", but only this one
// can tell it that config writes here must be atomic because a half-written
// file once bricked the launcher. Retrieved memories become constraints in the
// generated prompt, carrying their reasons.
//
// Everything here is pure. The store call and the LLM call live in the caller,
// so the shape of the prompt -- which is the part that decides whether the
// output is worth pasting -- can be tested without either.
package compose

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Emphases are the quality axes a caller can ask for.
const (
	EmphasisFlow      = "flow"
	EmphasisStructure = "structure"
	EmphasisCoding    = "coding"
	EmphasisUX        = "ux"
	EmphasisSecurity  = "security"
	EmphasisCost      = "cost"
)

// Emphases lists every valid value, in the order fragments are emitted.
var Emphases = []string{
	EmphasisFlow, EmphasisStructure, EmphasisCoding,
	EmphasisUX, EmphasisSecurity, EmphasisCost,
}

// fragments are appended to the FOCUS block, one per selected emphasis.
//
// Each has to change the output. "Write good UX" is an adjective and buys
// nothing; "name which of loading, empty, invalid, failed and saved the screen
// must show distinctly" is a line the model can fail to satisfy.
var fragments = map[string]string{
	EmphasisFlow: "flow — Order the Build items so nothing starts before what it depends on, and say in the item which earlier item it needs. Add one Done-when line covering what the user sees when the operation fails or is cancelled partway through.",

	EmphasisStructure: "structure — In each Build item, say where the new code goes relative to what already exists. Add one Constraint: no new dependency, module, or layer of abstraction unless the Build item that needs it states the reason on the same line.",

	EmphasisCoding: "coding — Turn the two most likely edge cases into Done-when lines, each naming the concrete input that triggers it. Add one Constraint: no TODO, no placeholder, no stub that returns a fixed value.",

	EmphasisUX: "ux — Add one Done-when line naming which of loading, empty, invalid input, failed, and saved the screen must show distinctly. Add one Done-when line for what happens to what the user typed when the action fails.",

	EmphasisSecurity: "security — Name the one trust boundary this specific change crosses, and add a Constraint that values arriving from the untrusted side are re-checked on the far side of it. No generic checklist, and no threat this task does not actually have.",

	EmphasisCost: "cost — Add a Constraint naming the files this change may touch and forbidding edits outside them. Add a Constraint: reply with the diff and at most three lines of summary, no reading file contents back, no narrating the process.",
}

// ValidEmphasis reports whether e is one of the six.
func ValidEmphasis(e string) bool {
	_, ok := fragments[e]
	return ok
}

// System is the instruction sent to the generating model.
//
// Two things in here are load-bearing and non-obvious.
//
// WRITE IT ONCE exists because the configured provider is a reasoning model
// that spends its completion budget thinking before it writes. An instruction
// that invites deliberation -- "consider several approaches", "review your
// draft" -- reliably exhausts the budget and returns nothing at all. That
// failure has already happened twice in this codebase.
//
// GROUNDING exists because a prompt that invents a framework is worse than one
// that says nothing: the coding model obeys an invented constraint instead of
// reading the repository, and the developer cannot tell which lines were
// grounded. Stating properties rather than mechanisms is the same rule applied
// to implementation advice.
const System = `You turn a developer's rough description into one finished prompt that they paste, unchanged, into the coding model named in the user message.

WRITE IT ONCE
The form below is fixed. You are filling it in, not designing it. Do not plan, outline, draft, critique, or revise. When two wordings look equally good, take the first and keep writing. Your reasoning is discarded and only the prompt is kept, so an unfinished prompt is worth nothing.

OUTPUT CONTRACT
Your first character is the prompt's first character. No preamble, no "Here is", no closing remark, no code fence around the whole reply. Everything you write is addressed to the coding model, never to the developer.

FORM
These blocks, this order. Skip any block you have nothing real for; an empty heading is paid for on every paste.

> Understood: <one sentence, in your words, restating what they asked for>

## Goal
One sentence: what exists when this is done.

## Context
Only facts you were given. Omit the heading if you were given none.

## Build
3-7 numbered items, each one concrete unit of work. No item restates another.

## Constraints
Hard rules, one line each, imperative. When a rule came to you with a reason, keep the reason in the same sentence. A rule whose cause is known gets followed; a bare rule gets traded away the first time it is inconvenient.

## Check first
Up to 3 lines, each of this form: Check <specific place in the repo>; if you cannot determine it, assume <X> and say so in one line. Never a question to the developer. Omit the heading if there is nothing real to put here.

## Not in this change
Up to 4 lines naming work a capable model would reasonably add here and should not. End any line the developer did not actually rule out with the exact word (assumed).

## Done when
3-5 lines. Each is an outcome someone can confirm by reading the diff or by running the thing, and at least one is a failure case. Never a restatement of Build or Constraints.

Then exactly this closing line, with no heading:
Report which Done-when lines you confirmed by reading the diff, and mark the rest not verified.

GROUNDING
Every fact you state must come from REQUEST, MEMORIES, or PROJECT. You know nothing else about this codebase, this machine, or this developer. Never name a framework, library, file, directory, command, version, or convention you were not given.
State properties, never mechanisms. Write what must be true — "a crash partway through a save must never leave a partly written file" — not how to do it. You do not know the operating system, the standard library, or what is already a dependency, and a wrong mechanism written as a rule gets obeyed instead of questioned.
The repo outranks you. Every assumption you make is a fallback for something the coding model can read for itself. Write it that way, never as an override.

MEMORIES
MEMORIES is this developer's own recorded lessons, decisions, preferences and facts. Each one you use becomes one Constraint line, in your own words, carrying its reason. Never quote one, never label it, never write "per your memory" or "project convention", never give them a section of their own, never mention that a memory system exists. Use each at most once: a memory becomes a Constraint or a Done-when line, never both. Silently drop every memory this task does not touch — a confident rule about the wrong thing costs more than a missing one. When MEMORIES is none, claim no conventions at all and make the first Check-first line: read the code next to this change and match what is already there.

FOCUS
The user message may list FOCUS lines. Each adds at most two lines to the blocks it names. Never a new heading, never a lecture, never a quality nobody asked for.

LANGUAGE
The request may be in imperfect English or another language. Read the intent and write clean, plain English. Never copy its grammar. Never translate or fix a domain word you do not recognise — carry it through exactly as written.

LENGTH
Under 450 words, and shorter when the task is small. When the user message says MODE: brief, emit only the Understood line, Goal, Constraints and Done when, under 150 words total. No praise, no role-play, no explaining why the prompt is good.

Output the prompt. Nothing else.`

// Memory is one retrieved memory, flattened to what the prompt needs.
type Memory struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"`
	Title   string  `json:"title"`
	Body    string  `json:"-"`
	Project string  `json:"project,omitempty"`
	Score   float64 `json:"score,omitempty"`
	Source  string  `json:"source"` // "context" or "search"
}

// Input is everything the user message is built from.
type Input struct {
	Task     string
	Project  string
	Target   string
	Mode     string // "brief" or "full"; Build fills it in when empty
	Emphases []string
	Memories []Memory
}

const (
	// DefaultTarget is what the prompt is written for when the caller says
	// nothing. The generated prompt names it, so it must not be blank.
	DefaultTarget = "Claude Opus 5"

	// MaxTask bounds the description. Above this it is not a description any
	// more, and the reasoning budget goes into reading it.
	MaxTask = 4000

	maxMemories    = 8
	maxMemoryChars = 1200
	maxMemoryBody  = 200
	maxTargetChars = 60
)

// sectionRE matches a line that would look like one of the user message's own
// section headings. Task text is user-controlled and is placed inside that
// message, so a line reading "MEMORIES" in the description could otherwise
// start a block the model reads as ours.
var sectionRE = regexp.MustCompile(`(?m)^[ \t]*(FOCUS|MEMORIES|REQUEST|MODE:|TARGET:|PROJECT:)[ \t]*$`)

// Build renders the user message.
func Build(in Input) string {
	target := oneLine(strings.TrimSpace(in.Target))
	if target == "" {
		target = DefaultTarget
	}
	target = truncate(target, maxTargetChars)

	mode := in.Mode
	if mode != "brief" && mode != "full" {
		mode = pickMode(in)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "TARGET: %s\n", target)
	// Omitted entirely when unknown. "PROJECT: (none)" is a placeholder, and a
	// placeholder is a fact the model will try to make use of.
	if p := oneLine(strings.TrimSpace(in.Project)); p != "" {
		fmt.Fprintf(&b, "PROJECT: %s\n", p)
	}
	fmt.Fprintf(&b, "MODE: %s\n", mode)

	b.WriteString("\nFOCUS\n")
	b.WriteString(strings.Join(focusLines(in.Emphases), "\n"))

	b.WriteString("\n\nMEMORIES\n")
	b.WriteString(renderMemories(in.Memories))

	b.WriteString("\n\nREQUEST\n")
	b.WriteString(sectionRE.ReplaceAllString(strings.TrimSpace(in.Task), " $1"))

	b.WriteString("\n\nWrite the finished prompt for TARGET now. Output only the prompt.\n")
	return b.String()
}

// pickMode chooses brief or full.
//
// Decided here rather than by the model: a branch inside a reasoning model is
// budget spent to reach a conclusion this code already has.
func pickMode(in Input) string {
	if len([]rune(strings.TrimSpace(in.Task))) < 120 && len(selected(in.Emphases)) <= 1 {
		return "brief"
	}
	return "full"
}

// focusLines returns the fragments for the selected emphases, in fixed order.
//
// Cost is always included, whether or not it was asked for. It is the only
// fragment that bounds what the generated prompt costs on every future paste,
// and making the cost control opt-in in a feature whose stated purpose is lower
// cost is the wrong default.
func focusLines(emphases []string) []string {
	want := map[string]bool{EmphasisCost: true}
	for _, e := range selected(emphases) {
		want[e] = true
	}
	var out []string
	for _, e := range Emphases {
		if want[e] {
			out = append(out, fragments[e])
		}
	}
	return out
}

func selected(emphases []string) []string {
	var out []string
	for _, e := range emphases {
		e = strings.ToLower(strings.TrimSpace(e))
		if ValidEmphasis(e) {
			out = append(out, e)
		}
	}
	return out
}

// NoMemories is what the block says when nothing was retrieved.
//
// Stated as an instruction rather than left blank: an empty block invites the
// model to fall back on what it imagines a project like this looks like, which
// is exactly the invention the grounding rule forbids.
const NoMemories = "(none — you know nothing about this codebase)"

// Select applies the caps and returns exactly the memories that will reach the
// model.
//
// Exported and used by the caller as well as by Build, so that what the API
// reports as "grounded in" is what was actually sent. Reporting the retrieved
// count instead overstates the grounding: retrieval happily returns seventeen
// memories, of which eight fit, and a developer auditing a constraint against a
// list containing nine memories the model never saw is being misled.
func Select(ms []Memory) []Memory {
	var out []Memory
	total := 0
	for _, m := range ms {
		if len(out) >= maxMemories {
			break
		}
		if total+len(memoryLine(m)) > maxMemoryChars {
			break
		}
		total += len(memoryLine(m))
		out = append(out, m)
	}
	return out
}

func memoryLine(m Memory) string {
	title := oneLine(m.Title)
	body := truncate(oneLine(m.Body), maxMemoryBody)
	line := "- " + title
	if body != "" && !strings.EqualFold(body, title) {
		line += ": " + body
	}
	return line
}

// renderMemories formats retrieved memories for the prompt.
//
// Deliberately stripped of ids, kinds, scores, dates and project names. Every
// one of those is a token the model pays for and then reasons about or echoes
// into the output; the developer sees provenance in the API response instead,
// where it costs nothing.
func renderMemories(ms []Memory) string {
	used := Select(ms)
	if len(used) == 0 {
		return NoMemories
	}
	lines := make([]string, 0, len(used))
	for _, m := range used {
		lines = append(lines, memoryLine(m))
	}
	return strings.Join(lines, "\n")
}

// Rank orders memories for the prompt: lessons first, then decisions,
// preferences and facts, each group by descending score.
//
// Lessons lead because they are already imperative and become a constraint
// verbatim. Facts trail because a fact is only worth its tokens when the task
// actually touches it.
func Rank(ms []Memory) []Memory {
	order := map[string]int{"lesson": 0, "decision": 1, "preference": 2, "fact": 3}
	out := append([]Memory(nil), ms...)
	sort.SliceStable(out, func(i, j int) bool {
		// The hand-written brief outranks everything, whatever its kind.
		// Ranked normally it loses twice over: it is stored as a preference,
		// which sorts third, and it is general by design, so it scores badly
		// against any specific task -- so the one piece of grounding the user
		// wrote themselves would be pushed out by the cap.
		bi, bj := out[i].Source == SourceBrief, out[j].Source == SourceBrief
		if bi != bj {
			return bi
		}
		oi, oj := order[out[i].Kind], order[out[j].Kind]
		if oi != oj {
			return oi < oj
		}
		return out[i].Score > out[j].Score
	})
	return out
}

// SourceBrief marks the project brief the user wrote by hand.
const SourceBrief = "brief"

// Dedupe drops memories already present, keeping the first occurrence.
//
// The two retrieval passes overlap by design -- standing conventions and
// task-specific hits are both wanted -- and a memory sent twice is paid for
// twice and reads as emphasis the developer did not intend.
func Dedupe(ms []Memory) []Memory {
	seen := map[string]bool{}
	var out []Memory
	for _, m := range ms {
		key := m.ID
		if key == "" {
			key = m.Kind + "\x00" + m.Title
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

var (
	fenceOpenRE  = regexp.MustCompile("^```[A-Za-z]*$")
	preambleRE   = regexp.MustCompile(`(?i)^(here('| i)s|sure[,!.]|certainly|of course|below is|i've|i have)\b`)
	understoodRE = regexp.MustCompile(`(?mi)^>?\s*Understood:\s*(.+)$`)
	assumedRE    = regexp.MustCompile(`(?mi)^\s*[-*]?\s*(.+?)\s*\(assumed\)\s*$`)
)

// Result is a generated prompt after the output contract has been enforced.
type Result struct {
	Prompt      string
	Understood  string
	Assumptions []string
}

// Normalise enforces the output contract in code rather than trusting it.
//
// The model is told not to wrap the prompt in a fence or open with "Here is
// your prompt", and mostly complies. Mostly is not a contract: the developer
// pastes this verbatim, so a stray fence becomes part of the prompt and a
// preamble becomes an instruction to the coding model.
func Normalise(text string) Result {
	s := strings.TrimSpace(text)

	// A fence wrapping the WHOLE reply, not one inside it -- inner fences are
	// legitimate example code and must survive.
	lines := strings.Split(s, "\n")
	first, last := firstNonEmpty(lines), lastNonEmpty(lines)
	if first >= 0 && last > first &&
		fenceOpenRE.MatchString(strings.TrimSpace(lines[first])) &&
		strings.TrimSpace(lines[last]) == "```" {
		lines = lines[first+1 : last]
		s = strings.TrimSpace(strings.Join(lines, "\n"))
	}

	// A one-line preamble before the prompt proper.
	lines = strings.Split(s, "\n")
	if i := firstNonEmpty(lines); i >= 0 && preambleRE.MatchString(strings.TrimSpace(lines[i])) {
		s = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
	}

	res := Result{Prompt: s}
	if m := understoodRE.FindStringSubmatch(s); m != nil {
		res.Understood = strings.TrimSpace(m[1])
	}
	for _, line := range strings.Split(s, "\n") {
		if m := assumedRE.FindStringSubmatch(line); m != nil {
			if a := strings.TrimSpace(m[1]); a != "" && !strings.HasPrefix(a, "#") {
				res.Assumptions = append(res.Assumptions, a)
			}
		}
	}
	return res
}

func firstNonEmpty(lines []string) int {
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			return i
		}
	}
	return -1
}

func lastNonEmpty(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return -1
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
