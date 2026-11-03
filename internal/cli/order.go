package cli

import (
	"bufio"
	"context"
	"strings"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/ui"
)

// firstLCN finds where a file starts on the disk (a variable for tests).
var firstLCN = copier.FirstLCN

// orderSmallFiles applies --order / copy_order to a local plan and returns
// the order used. "auto" means disk order for an NTFS source with a seek
// penalty (a spinning disk), else v1's largest-first order.
func orderSmallFiles(ctx context.Context, out *bufio.Writer, plan *copier.Plan, src, order string) string {
	asked := order
	if order == "auto" {
		order = copier.OrderSize
		if penalty, known := seekPenaltyFor(src); known && penalty {
			if fs, err := copyChecks().FileSystem(src); err == nil && strings.EqualFold(fs, "NTFS") {
				order = copier.OrderDisk
			}
		}
	}
	if order == copier.OrderSize {
		return order
	}
	c := plan.Counts()
	used, why := plan.OrderSmallFiles(ctx, order, firstLCN, copier.DiskOrderBudget(c.Files, c.WriteBytes))
	switch {
	case used == copier.OrderDisk:
		out.WriteString(ui.Dim("Reading small files in the order they sit on the disk (fewer head movements).") + "\n")
	case why != "" && (asked == copier.OrderDisk || asked == "auto"):
		out.WriteString(ui.Dim("Disk order not used: "+why+".") + "\n")
	}
	return used
}
