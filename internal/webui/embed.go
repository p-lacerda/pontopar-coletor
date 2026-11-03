package webui

import _ "embed"

// dashboardHTML é a página única do painel. Os marcadores {{TOKEN}} e {{VERSION}}
// são substituídos pelo servidor a cada GET /.
//
//go:embed dashboard.html
var dashboardHTML string
