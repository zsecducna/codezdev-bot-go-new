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
// catalog_buy.go, catalog_deposit.go, catalog_pay.go, catalog_apikey.go, and
// catalog_customer.go (M2.2-M2.6) have all since landed — struck from the
// deferred list below, which is now current as of the admin-panel-port audit.
//
// Deferred clusters (Python constant regions in telegram_bot.py), owned by the
// noted porting slices from handlers-go.md §7:
//
//   catalog_admin.go     — admin panel / dashboard / settings strings    (M3.x)
//   catalog_entra.go     — Entra / provpool admin strings                (M3.4)
//
// (AWS-sell admin strings were struck entirely, not deferred — AWS-sell was
// cut from scope, see .omc/ADMIN_PORT_HANDOFF.md §1.)
//
// Intentionally no code here — this file documents the seam so a later slice
// knows exactly where its strings attach.
