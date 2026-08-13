package handlers

// CanonicalCaptureOrder is the exact source order of the text-capture chain
// (handlers-go.md §2.2 table; telegram_bot.py:17220-18676). Order IS the
// contract: the chain is append-only and a golden test pins whatever lands
// against this list as an in-order SUBSEQUENCE, so slices can land captures
// independently without breaking precedence (handlers-go.md §7 rule).
//
// M2.1 registers NONE of these — the chain is empty. The list is declared now
// so the golden test and every later slice share one authoritative ordering.
var CanonicalCaptureOrder = []string{
	"upstock",      // 1  @17220 (A) peek
	"reset",        // 2  @17237 (A) peek
	"entra",        // 3  @17257 peek
	"getord",       // 4  @17518 pop (py_orders_spec.md: pops BEFORE validation, invalid ID aborts)
	"custlookup",   // 5  @17561 pop
	"addadmin",     // 6  @17581 pop
	"addreseller",  // 7  @17606 pop
	"addwl",        // 8  @17630 pop
	"rmadmin",      // 9  @17655 pop
	"setrate",      // 10 @17675 pop
	"setapiprice",  // 11 @17696 pop
	"setapimin",    // 12 @17730 pop
	"setapimax",    // 13 @17770 pop
	"setkiroprice", // 14 @17792 pop
	"setkiromin",   // 15 @17822 pop
	"setkiromax",   // 16 @17855 pop
	"setapicap",    // 17 @17874 (S) pop
	"setkirourl",   // 18 @17895 (S) pop
	"setkirokey",   // 19 @17927 (S) pop
	"setkiroacct",  // 20 @17953 (S) pop
	"setbep20",     // 21 @17979 pop
	"settron",      // 22 @17997 pop
	"setmaxqty",    // 23 @18015 pop
	"refundreject", // 26 @18086 pop (py_orders_spec.md: any typed line commits, no re-arm on "invalid")
	"newprod",      // 27 @18108 pop
	"renprod",      // 28 @18146 pop
	// provpool (NEW — no Python original, py_stock_spec.md §6.2): Python wires
	// pending_provpool a FILE handler only (@17607) and never a matching
	// paste/text handler despite its own prompt inviting one — a confirmed
	// latent gap (§6.2's "PASTE-AS-TEXT IS NOT [wired]" note). This Go port
	// fixes it rather than replicating the dead end: the paste path mirrors
	// the file handler's ATOMIC POP-CLAIM (single-shot batch kickoff, not an
	// accumulating preview like "upstock"), a full-admin-only provisioning
	// flow in this category, with no canonical Python line to cite.
	"provpool",          // 29 (Go-only)
	"setnotice",         // 31 @18174 pop
	"setdelivery",       // 32 @18192 pop
	"editfile",          // 33 @18209 cancel
	"refund",            // 34 @18237 pop (py_orders_spec.md: pops BEFORE validation, invalid ID aborts)
	"discount",          // 35 @18250 pop (abort-on-invalid: py_pricing_spec.md §2)
	"reseller_discount", // 36 @18288 peek
	"promo",             // 37 @18310 pop (abort-on-invalid: py_pricing_spec.md §3)
	"code",              // 38 @18341 peek
	"custreset",         // 39 @18350 peek
	"custdelete",        // 40 @18398 peek
	"hold",              // 41 @18437 peek (RE-ARMS on invalid quantity — py_stock_spec.md §3.4/§11)
	"tier",              // 42 @18464 peek
	"price",             // 43 @18499 peek
	"deposit",           // 46 @18586 peek
	"apikey",            // 47 @18614 peek
	"apikeyrc",          // 48 @18640 peek
	"ordercode",         // 49 @18664 peek
	"purchase",          // 50 @18676 peek (buy qty — deliberately LAST)
}

// addbal (adm:addbal, telegram_bot.py:17223-17240/17243-17273) is NOT a
// captureChain entry (M3/M4 fix, adminusers.go): it moved into the shared
// step-9 "admininput" stage (adminsettings.go's capAdminInput,
// CanonicalAdminTextOrder #10) alongside "broadcast" — matching Python's real
// structure, where every ADMIN_ACTIONS member's admin_collect_input call sits
// in the SAME pending_admin bucket, not the step-10 text-capture chain. A
// dedicated captureChain slot for it (as this list used to carry) was an
// artifact of an earlier task's file-ownership split, not a Python fact.

// captureNames returns the names of the currently-registered capture-chain
// stages, in order — the input to TestCaptureChainOrder.
func (b *Bot) captureNames() []string { return stageNames(b.captureChain) }

// fileInterceptNames / adminTextNames expose the other two ordered stages so
// their tests can assert order too once they carry entries.
func (b *Bot) fileInterceptNames() []string { return stageNames(b.fileIntercepts) }
func (b *Bot) adminTextNames() []string     { return stageNames(b.adminText) }

func stageNames(s []stage) []string {
	out := make([]string, len(s))
	for i, st := range s {
		out[i] = st.name
	}
	return out
}
