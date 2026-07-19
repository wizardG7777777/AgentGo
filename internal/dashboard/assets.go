package dashboard

import _ "embed"

// indexHTML 是内嵌的单页应用（inline CSS+JS，无任何外部 CDN 依赖，可离线工作）。
//
//go:embed assets/index.html
var indexHTML []byte
