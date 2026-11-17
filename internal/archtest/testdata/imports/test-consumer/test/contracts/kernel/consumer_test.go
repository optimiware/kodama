package kernel_test

import (
	_ "database/sql"
	_ "net/http"

	_ "example.com/testify/require"

	_ "kodema/internal/id"
	_ "kodema/internal/kernel"
	_ "kodema/internal/shell/store/postgres"
)
