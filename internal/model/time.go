package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Time member naming: "2025" (year) → "2025Q1".."2025Q4" → "2025M1".."2025M12".
// Only month members store data.

// TimeName formats a month member name.
func TimeName(year, month int) string { return fmt.Sprintf("%dM%d", year, month) }

// ParseTimeMonth parses "2025M7" → (2025, 7, true).
func ParseTimeMonth(name string) (year, month int, ok bool) {
	i := strings.IndexByte(name, 'M')
	if i <= 0 {
		return 0, 0, false
	}
	y, err1 := strconv.Atoi(name[:i])
	m, err2 := strconv.Atoi(name[i+1:])
	if err1 != nil || err2 != nil || m < 1 || m > 12 || y < 1 || strings.Contains(name[i+1:], "Q") {
		return 0, 0, false
	}
	return y, m, true
}

// TimeIsMonth reports whether name is a month member.
func TimeIsMonth(name string) bool { _, _, ok := ParseTimeMonth(name); return ok }

// MonthIndex returns a sortable index (year*12 + month-1) for a month member,
// or -1 if name is not a month.
func MonthIndex(name string) int {
	y, m, ok := ParseTimeMonth(name)
	if !ok {
		return -1
	}
	return y*12 + m - 1
}

// PriorMonth returns the month before (2025M1 → 2024M12).
func PriorMonth(name string) (string, bool) {
	y, m, ok := ParseTimeMonth(name)
	if !ok {
		return "", false
	}
	if m == 1 {
		return TimeName(y-1, 12), true
	}
	return TimeName(y, m-1), true
}

// YTDMonths returns the months from January through name's month, same year.
func YTDMonths(name string) []string {
	y, m, ok := ParseTimeMonth(name)
	if !ok {
		return nil
	}
	out := make([]string, 0, m)
	for i := 1; i <= m; i++ {
		out = append(out, TimeName(y, i))
	}
	return out
}

// MonthsUnder returns the month members under any time member (a month maps
// to itself), in chronological order. Returns nil for unknown members.
func MonthsUnder(d *Dimension, name string) []string {
	if TimeIsMonth(name) {
		if d.Has(name) {
			return []string{name}
		}
		return nil
	}
	leaves := d.Leaves(name)
	months := leaves[:0:0]
	for _, l := range leaves {
		if TimeIsMonth(l) {
			months = append(months, l)
		}
	}
	sort.Slice(months, func(i, j int) bool { return MonthIndex(months[i]) < MonthIndex(months[j]) })
	return months
}

// BuildTimeDim generates the Time dimension for the given years.
func BuildTimeDim(years []int) *Dimension {
	d := NewDimension(DimTime)
	sorted := append([]int(nil), years...)
	sort.Ints(sorted)
	for _, y := range sorted {
		AddTimeYear(d, y)
	}
	return d
}

// AddTimeYear adds one year (with quarters and months) to a Time dimension.
// Adding an existing year is a no-op.
func AddTimeYear(d *Dimension, year int) {
	yname := strconv.Itoa(year)
	if d.Has(yname) {
		return
	}
	_ = d.AddMember(&Member{Name: yname})
	for q := 1; q <= 4; q++ {
		qname := fmt.Sprintf("%dQ%d", year, q)
		_ = d.AddMember(&Member{Name: qname, Parent: yname})
		for m := (q-1)*3 + 1; m <= q*3; m++ {
			_ = d.AddMember(&Member{Name: TimeName(year, m), Parent: qname})
		}
	}
}

// TimeYears returns the year numbers present in a Time dimension, ascending.
func TimeYears(d *Dimension) []int {
	var years []int
	for _, r := range d.Roots {
		if y, err := strconv.Atoi(r); err == nil {
			years = append(years, y)
		}
	}
	sort.Ints(years)
	return years
}
