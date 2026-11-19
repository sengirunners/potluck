package cli

import (
	"context"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/pushbase"
)

func (p pushFlags) hostBase(ctx context.Context, src *boxpush.Source) *pushbase.Plan {
	if p.noBase || p.full || src == nil {
		return nil
	}
	repo, err := pushbase.Identify(ctx, src)
	if err != nil || repo == nil {
		return nil
	} // cache discovery must not prevent the push
	return &pushbase.Plan{Repository: repo, Dir: src.Repo, Max: int64(p.maxMiB) << 20}
}
