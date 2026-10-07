package core

// Order slicing. NSE caps single-order quantity (freeze qty, per
// instrument, changes by circular — NIFTY went 1800 -> 3510 on
// 2026-10-05). The slicer derives slices from the instrument's
// current FreezeQty — never from a hardcoded number.

// SlicePlan describes how a total quantity will be split.
type SlicePlan struct {
	// Qty per slice, each <= freeze.
	Quantities []int
	// FreezeQty used for the computation.
	FreezeQty int
}

// PlanSlices splits total lots into child quantities, fire-all style.
// Each slice is the maximum allowed (FreezeQty), remainder last:
// NIFTY 100 lots = 6500 units, freeze 3510 -> [3510, 2990].
func PlanSlices(inst Instrument, totalLots int) SlicePlan {
	if totalLots <= 0 || inst.LotSize <= 0 {
		return SlicePlan{}
	}
	total := totalLots * inst.LotSize
	freeze := inst.FreezeQty
	if freeze <= 0 {
		// unknown freeze: single slice of the whole order (adapter
		// will surface a broker rejection if truly over-limit)
		return SlicePlan{Quantities: []int{total}, FreezeQty: 0}
	}
	if total <= freeze {
		return SlicePlan{Quantities: []int{total}, FreezeQty: freeze}
	}
	var qs []int
	remaining := total
	for remaining > 0 {
		q := freeze
		if remaining < freeze {
			q = remaining
		}
		qs = append(qs, q)
		remaining -= q
	}
	return SlicePlan{Quantities: qs, FreezeQty: freeze}
}

// SplitLots caps a lot count at the per-order maximum and reports
// the overflow (used by the risk-cap validation).
func SplitLots(inst Instrument, lots int) (within int, overflow int) {
	max := inst.MaxLotsPerOrder()
	if max <= 0 || lots <= max {
		return lots, 0
	}
	return max, lots - max
}