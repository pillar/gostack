// Package templates embeds the project skeleton used by `gostack init`.
package templates

import "embed"

// App is the application skeleton; files ending in .tmpl are rendered with text/template.
//
//go:embed all:app
var App embed.FS
