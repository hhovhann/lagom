// Package workload generates the synthetic, machine-checkable task mix used by
// the eval harness and the live demo page.
package sim

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
)

// Task is one unit of work with a machine-checkable outcome. The verifier spec
// stands in for the customer's real success signal (test passed, schema valid,
// ticket resolved...). Gold/Wrong are only used by the mock LLM simulator.
type Task struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Prompt string `json:"prompt"`
	Verify string `json:"verify"`
	Gold   string `json:"gold"`
	Wrong  string `json:"wrong"`
}

var products = []string{"Atlas", "Pulse", "Beacon", "Nimbus"}

var ticketTemplates = map[string][]string{
	"billing": {
		"I was charged twice for my %s subscription this month, please refund one of the payments.",
		"My last invoice for %s shows an amount higher than my plan price. Can you correct it?",
		"My payment for %s failed and I need to update the credit card on file.",
	},
	"bug": {
		"The %s dashboard shows a blank screen and then crashes whenever I click Export.",
		"Search in %s returns an error 500 since the latest update.",
		"Push notifications from %s stopped appearing on my Android phone yesterday.",
	},
	"feature_request": {
		"It would be great if %s had a dark mode option.",
		"Could you add CSV export to the reports page in %s?",
		"Please consider adding single sign-on with Okta to %s.",
	},
	"account_access": {
		"I cannot log in to %s: the password reset email never arrives.",
		"My %s account is locked after too many attempts, please unlock it.",
		"I lost my two-factor device and need to regain access to my %s account.",
	},
}

var labelOrder = []string{"billing", "bug", "feature_request", "account_access"}

var vendors = []string{"Northwind Traders", "Acme Industrial", "Blue Harbor Logistics", "Kestrel Software", "Orchard & Pine"}
var currencies = []string{"EUR", "USD", "GBP"}

var faqs = []struct{ q, a string }{
	{"What is the capital of France?", "Paris"},
	{"What is the capital of Japan?", "Tokyo"},
	{"What is the capital of Canada?", "Ottawa"},
	{"What is the capital of Australia?", "Canberra"},
	{"What is the capital of Brazil?", "Brasilia"},
	{"What is the capital of Egypt?", "Cairo"},
	{"What is the chemical symbol for gold?", "Au"},
	{"What is the chemical symbol for sodium?", "Na"},
	{"What is the largest planet in our solar system?", "Jupiter"},
	{"What is the smallest prime number?", "2"},
	{"How many days are in a leap year?", "366"},
	{"What is the boiling point of water at sea level in degrees Celsius?", "100"},
}

func commas(n int) string {
	s := fmt.Sprint(n)
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)
	return strings.Join(out, ",")
}

// Generate returns n deterministic tasks for a seed.
func Generate(n int, seed uint64) []Task {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	tasks := make([]Task, 0, n)
	for i := 0; i < n; i++ {
		var t Task
		switch x := r.IntN(100); {
		case x < 28:
			t = genClassify(r)
		case x < 43:
			t = genExtract(r)
		case x < 58:
			t = genReason(r)
		case x < 68:
			t = genFAQ(r)
		case x < 84:
			t = genGrounded(r)
		default:
			t = genSummary(r)
		}
		t.ID = fmt.Sprintf("t%04d", i)
		tasks = append(tasks, t)
	}
	return tasks
}

func genClassify(r *rand.Rand) Task {
	label := labelOrder[r.IntN(len(labelOrder))]
	tpl := ticketTemplates[label]
	text := fmt.Sprintf("[Ticket #%d] ", 10000+r.IntN(90000)) +
		fmt.Sprintf(tpl[r.IntN(len(tpl))], products[r.IntN(len(products))])
	wrong := labelOrder[(indexOf(labelOrder, label)+1+r.IntN(3))%len(labelOrder)]
	return Task{
		Type: "classify",
		Prompt: "Classify the support ticket into exactly one of: billing, bug, feature_request, account_access. " +
			"Reply with only the label.\n\nTicket: " + text,
		Verify: "equals:" + label, Gold: label, Wrong: wrong,
	}
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return 0
}

func genExtract(r *rand.Rand) Task {
	vendor := vendors[r.IntN(len(vendors))]
	inv := fmt.Sprintf("INV-%04d", 1000+r.IntN(9000))
	cur := currencies[r.IntN(len(currencies))]
	whole := 100 + r.IntN(9900)
	cents := r.IntN(100)
	total := float64(whole) + float64(cents)/100
	date := fmt.Sprintf("2026-%02d-%02d", 1+r.IntN(12), 1+r.IntN(28))
	text := fmt.Sprintf("Invoice %s from %s dated %s. Total due: %s %s.%02d. Payment terms net %d.",
		inv, vendor, date, cur, commas(whole), cents, []int{15, 30, 45, 60}[r.IntN(4)])
	gold := map[string]any{"vendor": vendor, "invoice_number": inv, "total": total, "currency": cur}
	wrong := map[string]any{"vendor": vendor, "invoice_number": inv, "total": total + 100, "currency": cur}
	gb, _ := json.Marshal(gold)
	wb, _ := json.Marshal(wrong)
	return Task{
		Type: "extract",
		Prompt: "Extract these fields from the invoice text and reply with a single JSON object only, no prose: " +
			"vendor (string), invoice_number (string), total (number, no thousands separators), currency (3-letter code).\n\nInvoice text: " + text,
		Verify: "jsoneq:" + string(gb), Gold: string(gb), Wrong: string(wb),
	}
}

func genReason(r *rand.Rand) Task {
	var prompt string
	var ans int
	switch r.IntN(3) {
	case 0:
		a, b, c, d := 800+r.IntN(1200), 20+r.IntN(60), 3+r.IntN(8), 100+r.IntN(400)
		ans = a - b*c + d
		prompt = fmt.Sprintf("A warehouse holds %d boxes. Each day %d boxes ship out, for %d days. Then a delivery adds %d boxes. "+
			"How many boxes are in the warehouse now? Reply with only the number.", a, b, c, d)
	case 1:
		p := []int{25, 50, 75, 100, 125}[r.IntN(5)]
		d := []int{10, 20, 25, 50}[r.IntN(4)]
		ans = 12 * p * (100 - d) / 100
		prompt = fmt.Sprintf("A subscription costs $%d per month. The annual plan covers 12 months with a %d%% discount on the total. "+
			"What is the annual plan price in dollars? Reply with only the number.", p, d)
	default:
		e, k, w, rr := 4+r.IntN(12), 3+r.IntN(9), 2+r.IntN(6), 5+r.IntN(40)
		ans = e*k*w - rr
		prompt = fmt.Sprintf("A team of %d engineers each close %d tickets per week. After %d weeks, %d of those tickets were reopened. "+
			"How many tickets remain closed? Reply with only the number.", e, k, w, rr)
	}
	return Task{
		Type: "reason", Prompt: prompt,
		Verify: fmt.Sprintf("number:%d", ans), Gold: fmt.Sprint(ans), Wrong: fmt.Sprint(ans + 7 + r.IntN(20)),
	}
}

func genFAQ(r *rand.Rand) Task {
	f := faqs[r.IntN(len(faqs))]
	return Task{
		Type: "faq", Prompt: f.q + " Reply with only the answer.",
		Verify: "equals:" + f.a, Gold: f.a, Wrong: "Unknown",
	}
}

// grounded: answer from a given passage (retrieval-style); success = the key fact is in the answer.
var passages = []struct{ text, q, fact string }{
	{"%s support is open Monday to Friday from 9:00 to 17:00 CET. Premium customers can also call on Saturday.", "On which weekend day can premium customers call %s support?", "Saturday"},
	{"The %s starter plan includes 5 seats. Each additional seat costs 12 euros per month.", "How much does each additional %s seat cost per month in euros? Reply with only the number.", "12"},
	{"%s keeps backups for 30 days. Deleted projects can be restored within that period.", "For how many days can a deleted %s project be restored? Reply with only the number.", "30"},
	{"%s signs in through Okta and Microsoft Entra. Google sign-in is planned for next year.", "Which provider is planned for next year for %s sign-in?", "Google"},
	{"Refunds for %s are issued to the original payment method within 5 business days.", "Within how many business days are %s refunds issued? Reply with only the number.", "5"},
}

func genGrounded(r *rand.Rand) Task {
	p := passages[r.IntN(len(passages))]
	prod := products[r.IntN(len(products))]
	return Task{
		Type: "grounded", Prompt: "Answer using only the passage.\n\nPassage: " + fmt.Sprintf(p.text, prod) + "\n\nQuestion: " + fmt.Sprintf(p.q, prod),
		Verify: "contains:" + p.fact, Gold: p.fact, Wrong: "The passage does not say.",
	}
}

// summary: an open-ended answer judged by hard gates (word limit, required term, banned claim).
func genSummary(r *rand.Rand) Task {
	label := labelOrder[r.IntN(len(labelOrder))]
	prod := products[r.IntN(len(products))]
	tpl := ticketTemplates[label]
	msg := fmt.Sprintf(tpl[r.IntN(len(tpl))], prod)
	return Task{
		Type:   "summary",
		Prompt: fmt.Sprintf("Summarise this customer message in at most 14 words. You must mention %q and you must not use the word \"guarantee\".\n\nMessage: %s", prod, msg),
		Verify: fmt.Sprintf("rules:maxwords=14;must=%s;ban=guarantee", prod),
		Gold:   prod + " customer reports a " + strings.ReplaceAll(label, "_", " ") + " issue.",
		Wrong:  "The customer wrote a very long message about many different things and we absolutely guarantee that everything will be fixed immediately.",
	}
}
