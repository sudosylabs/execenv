package wire

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

func FileInfoTo(f execenv.FileInfo) *execenvv1.FileInfo {
	return &execenvv1.FileInfo{
		Path:       f.Path,
		Dir:        f.Dir,
		Executable: f.Executable,
		Size:       f.Size,
		Modified:   timestamppb.New(f.Modified),
		Sha256:     f.SHA256,
	}
}

func FileInfoFrom(f *execenvv1.FileInfo) execenv.FileInfo {
	return execenv.FileInfo{
		Path:       f.GetPath(),
		Dir:        f.GetDir(),
		Executable: f.GetExecutable(),
		Size:       f.GetSize(),
		Modified:   f.GetModified().AsTime(),
		SHA256:     f.GetSha256(),
	}
}

func FileInfosTo(fs []execenv.FileInfo) []*execenvv1.FileInfo {
	out := make([]*execenvv1.FileInfo, 0, len(fs))
	for _, f := range fs {
		out = append(out, FileInfoTo(f))
	}
	return out
}

func FileInfosFrom(fs []*execenvv1.FileInfo) []execenv.FileInfo {
	out := make([]execenv.FileInfo, 0, len(fs))
	for _, f := range fs {
		out = append(out, FileInfoFrom(f))
	}
	return out
}
