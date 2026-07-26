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
	"upstock",           // 1  @17220 (A) peek
	"reset",             // 2  @17237 (A) peek
	"entra",             // 3  @17257 peek
	"getord",            // 4  @17518 peek
	"custlookup",        // 5  @17561 pop
	"addadmin",          // 6  @17581 pop
	"addreseller",       // 7  @17606 pop
	"addwl",             // 8  @17630 pop
	"rmadmin",           // 9  @17655 pop
	"setrate",           // 10 @17675 pop
	"setapiprice",       // 11 @17696 pop
	"setapimin",         // 12 @17730 pop
	"setapimax",         // 13 @17770 pop
	"setkiroprice",      // 14 @17792 pop
	"setkiromin",        // 15 @17822 pop
	"setkiromax",        // 16 @17855 pop
	"setapicap",         // 17 @17874 (S) pop
	"setkirourl",        // 18 @17895 (S) pop
	"setkirokey",        // 19 @17927 (S) pop
	"setkiroacct",       // 20 @17953 (S) pop
	"setbep20",          // 21 @17979 pop
	"settron",           // 22 @17997 pop
	"setmaxqty",         // 23 @18015 pop
	"setawsprice",       // 24 @18035 peek
	"awsreject",         // 25 @18061 peek
	"refundreject",      // 26 @18086 peek
	"newprod",           // 27 @18108 pop
	"renprod",           // 28 @18146 pop
	"setnotice",         // 29 @18174 pop
	"setdelivery",       // 30 @18192 pop
	"editfile",          // 31 @18209 cancel
	"refund",            // 32 @18237 peek
	"discount",          // 33 @18250 peek
	"reseller_discount", // 34 @18288 peek
	"promo",             // 35 @18310 peek
	"code",              // 36 @18341 peek
	"custreset",         // 37 @18350 peek
	"custdelete",        // 38 @18398 peek
	"hold",              // 39 @18437 peek
	"tier",              // 40 @18464 peek
	"price",             // 41 @18499 peek
	"sell",              // 42 @18526 peek
	"txid",              // 43 @18534 peek
	"deposit",           // 44 @18586 peek
	"apikey",            // 45 @18614 peek
	"apikeyrc",          // 46 @18640 peek
	"ordercode",         // 47 @18664 peek
	"purchase",          // 48 @18676 peek (buy qty — deliberately LAST)
}

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
