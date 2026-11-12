package migrate

// QuotePasswordForTest exposes the SQL literal rendering to the external test
// package so the exact emitted bytes (the injection guard) are pinned.
func QuotePasswordForTest(password string) string { return quotePassword(password) }

// QuoteIdentForTest exposes the identifier quoting to the external test
// package so the doubled-quote form used for the administrator and database
// names is pinned.
func QuoteIdentForTest(name string) string { return quoteIdent(name) }
