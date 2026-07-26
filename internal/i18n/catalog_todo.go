package i18n

// catalog_todo.go — PLACEHOLDER for the feature-string clusters NOT yet ported.
//
// handlers-go.md §5 + §7 split the string tables by feature, landing with the
// slice that uses them. Each later slice ADDS its constants as new fields of
// Catalog (in a dedicated catalog_<feature>.go with the SAME Python identifier
// names) and populates them inside buildCatalog(). The completeness test
// (TestStringsComplete) walks whatever fields exist, so an unported cluster is
// simply absent — never a failing empty Str.
//
// Deferred clusters (Python constant regions in telegram_bot.py), owned by the
// noted porting slices from handlers-go.md §7:
//
//   catalog_buy.go       — PRODUCTS_HEADER, NO_PRODUCTS, OUT_OF_STOCK, tier /
//                          quantity / pay-screen / cancel-keep strings   (M2.2)
//   catalog_deposit.go   — DEPOSIT_QR, DEPOSIT_OK, DEPOSIT_TIMEOUT, /info
//                          BALANCE, history HISTORY_* strings            (M2.3)
//   catalog_pay.go       — PayPal / Binance / BEP20 / TRON prompts       (M2.4)
//   catalog_apikey.go    — APIKEY_BTN, KIROAPI_BTN, APIKEY_RC_BTN, mint /
//                          recharge / my-keys / group-refund strings     (M2.5)
//   catalog_customer.go  — Self-Service submenu headers, OTP, refund,
//                          cust reset/delete, sell-AWS intake strings    (M2.6)
//   catalog_admin.go     — admin panel / dashboard / settings strings    (M3.x)
//   catalog_entra.go     — Entra / provpool / AWS-sell admin strings     (M3.4)
//
// Intentionally no code here — this file documents the seam so a later slice
// knows exactly where its strings attach.
