package runtime

import (
	"cmp"
	"slices"
	"strconv"
)

// PickContainer returns the container whose log a replica's logs request
// reads: a running one first, then the highest hivemind.generation label, then
// the highest name. cs must not be empty.
func PickContainer(cs []Container) Container {
	gen := func(c Container) int {
		n, _ := strconv.Atoi(c.Labels[LabelGeneration])
		return n
	}
	return slices.MaxFunc(cs, func(a, b Container) int {
		if a.Running() != b.Running() {
			if a.Running() {
				return 1
			}
			return -1
		}
		return cmp.Or(cmp.Compare(gen(a), gen(b)), cmp.Compare(a.Name, b.Name))
	})
}
