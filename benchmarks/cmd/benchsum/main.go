// Command benchsum summarises `go test -bench -count N` output: for every
// benchmark it prints min / median / max of ns/op, B/op and allocs/op and the
// spread ((max-min)/median), as a Markdown table, and with -json as JSON.
//
//	go test ./driver -bench . -benchmem -count 5 | tee raw.txt | benchsum
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

type stat struct {
	Name     string    `json:"name"`
	Runs     int       `json:"runs"`
	NsOp     []float64 `json:"ns_op"`
	BOp      []float64 `json:"b_op"`
	AllocsOp []float64 `json:"allocs_op"`
	Min      float64   `json:"ns_op_min"`
	Median   float64   `json:"ns_op_median"`
	Max      float64   `json:"ns_op_max"`
	Spread   float64   `json:"ns_op_spread"`
	Allocs   float64   `json:"allocs_op_median"`
	Bytes    float64   `json:"b_op_median"`
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func main() {
	asJSON := flag.Bool("json", false, "JSON output")
	flag.Parse()
	m := map[string]*stat{}
	var order []string
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !strings.HasPrefix(f[0], "Benchmark") {
			continue
		}
		s := m[f[0]]
		if s == nil {
			s = &stat{Name: f[0]}
			m[f[0]] = s
			order = append(order, f[0])
		}
		for i := 2; i+1 < len(f); i += 2 {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				continue
			}
			switch f[i+1] {
			case "ns/op":
				s.NsOp = append(s.NsOp, v)
			case "B/op":
				s.BOp = append(s.BOp, v)
			case "allocs/op":
				s.AllocsOp = append(s.AllocsOp, v)
			}
		}
	}
	var out []*stat
	for _, n := range order {
		s := m[n]
		s.Runs = len(s.NsOp)
		if s.Runs == 0 {
			continue
		}
		s.Min, s.Max, s.Median = slices.Min(s.NsOp), slices.Max(s.NsOp), median(s.NsOp)
		s.Spread = (s.Max - s.Min) / s.Median
		s.Allocs, s.Bytes = median(s.AllocsOp), median(s.BOp)
		out = append(out, s)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return
	}
	fmt.Println("| benchmark | runs | ns/op min | ns/op median | ns/op max | spread | B/op | allocs/op |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	for _, s := range out {
		fmt.Printf("| %s | %d | %.0f | %.0f | %.0f | %.1f%% | %.0f | %.0f |\n", strings.TrimPrefix(s.Name, "Benchmark"), s.Runs, s.Min, s.Median, s.Max, s.Spread*100, s.Bytes, s.Allocs)
	}
}
