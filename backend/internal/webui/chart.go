package webui

import (
	"fmt"
	"html/template"
	"math"
	"strings"
)

// chartPoint is one bucket of the traffic series.
type chartPoint struct {
	T    int64 // unix seconds
	Up   int64
	Down int64
}

// areaChart renders the monochrome SVG area chart (port of the old
// components/AreaChart.vue: 720x220 viewBox, linear axes, dashed up series).
func areaChart(points []chartPoint, uid string) template.HTML {
	const (
		w         = 720.0
		h         = 220.0
		padL      = 46.0
		padR      = 8.0
		padT      = 10.0
		padB      = 20.0
		tickCount = 4
	)
	var ticks strings.Builder
	if len(points) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, `<div class="chart"><svg viewBox="0 0 %v %v" preserveAspectRatio="none">`, w, h)
		for i := 0; i <= tickCount; i++ {
			vy := h - padB - (float64(i)/float64(tickCount))*(h-padT-padB)
			fmt.Fprintf(&b, `<line x1="%v" x2="%v" y1="%.1f" y2="%.1f" class="grid-line"/>`, padL, w-padR, vy, vy)
			fmt.Fprintf(&b, `<text x="%v" y="%.1f" text-anchor="end" font-size="10" class="axis-text">0 B</text>`, padL-6, vy+3)
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="12" class="axis-text">暂无流量数据</text>`, w/2, h/2)
		b.WriteString(`</svg></div>`)
		return template.HTML(b.String())
	}
	xMin := points[0].T
	xMax := points[len(points)-1].T
	if xMax < xMin {
		xMin, xMax = xMax, xMin
	}
	yMax := 1.0
	for _, p := range points {
		if float64(p.Up) > yMax {
			yMax = float64(p.Up)
		}
		if float64(p.Down) > yMax {
			yMax = float64(p.Down)
		}
	}
	yMax *= 1.1

	x := func(t int64) float64 {
		if xMax == xMin {
			return padL
		}
		return padL + (float64(t-xMin)/float64(xMax-xMin))*(w-padL-padR)
	}
	y := func(v float64) float64 {
		return h - padB - (v/yMax)*(h-padT-padB)
	}

	path := func(key func(chartPoint) int64) string {
		var b strings.Builder
		for i, p := range points {
			verb := "L"
			if i == 0 {
				verb = "M"
			}
			fmt.Fprintf(&b, "%s%.1f,%.1f ", verb, x(p.T), y(float64(key(p))))
		}
		return b.String()
	}
	upPath := path(func(p chartPoint) int64 { return p.Up })
	downPath := path(func(p chartPoint) int64 { return p.Down })
	base := h - padB
	upArea := fmt.Sprintf("%s L%.1f,%.1f L%.1f,%.1f Z", upPath, x(points[len(points)-1].T), base, x(points[0].T), base)
	downArea := fmt.Sprintf("%s L%.1f,%.1f L%.1f,%.1f Z", downPath, x(points[len(points)-1].T), base, x(points[0].T), base)

	for i := 0; i <= tickCount; i++ {
		v := (yMax / tickCount) * float64(i)
		fmt.Fprintf(&ticks,
			`<line x1="%v" x2="%v" y1="%.1f" y2="%.1f" class="grid-line"/>`+
				`<text x="%v" y="%.1f" text-anchor="end" font-size="10" class="axis-text">%s</text>`,
			padL, w-padR, y(v), y(v), padL-6, y(v)+3, fmtBytesTrimmed(v))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<div class="chart"><svg viewBox="0 0 %v %v" preserveAspectRatio="none" role="img" aria-label="流量趋势图">`, w, h)
	fmt.Fprintf(&b, `<defs>`+
		`<linearGradient id="upFill-%s" x1="0" y1="0" x2="0" y2="1">`+
		`<stop offset="0%%" stop-color="currentColor" stop-opacity="0.22"/><stop offset="100%%" stop-color="currentColor" stop-opacity="0.01"/></linearGradient>`+
		`<linearGradient id="downFill-%s" x1="0" y1="0" x2="0" y2="1">`+
		`<stop offset="0%%" stop-color="currentColor" stop-opacity="0.4"/><stop offset="100%%" stop-color="currentColor" stop-opacity="0.03"/></linearGradient>`+
		`</defs>`, uid, uid)
	b.WriteString(ticks.String())
	fmt.Fprintf(&b, `<path d="%s" fill="url(#downFill-%s)"/>`, downArea, uid)
	fmt.Fprintf(&b, `<path d="%s" fill="none" class="line-down" stroke-width="1.8"/>`, downPath)
	fmt.Fprintf(&b, `<path d="%s" fill="url(#upFill-%s)" class="up-series"/>`, upArea, uid)
	fmt.Fprintf(&b, `<path d="%s" fill="none" class="line-up" stroke-width="1.4" stroke-dasharray="5 3"/>`, upPath)
	b.WriteString(`</svg><div class="legend"><span><i class="dot down"></i>下行 (DOWN)</span><span><i class="dot up"></i>上行 (UP)</span></div></div>`)
	return template.HTML(b.String())
}

// fmtBytesTrimmed formats axis tick values without the trailing unit for
// large magnitudes (mirrors the old fmtBytes(v, 0) usage).
func fmtBytesTrimmed(v float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	digits := 1
	if i == 0 || v >= 100 {
		digits = 0
	} else if v >= 10 {
		digits = 1
	}
	return fmt.Sprintf("%.*f %s", digits, math.Round(v*10)/10, units[i])
}
