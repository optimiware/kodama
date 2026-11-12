package kernel_test

import (
	_ "database/sql"
	_ "net/http"

	_ "example.com/testify/require"

	_ "kodama/internal/id"
	_ "kodama/internal/kernel"
	_ "kodama/internal/shell/store/postgres"
)
