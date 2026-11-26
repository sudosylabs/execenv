package wire

import (
	"io"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

func ImportChunks(receive func() bool, msg func() *execenvv1.ImportRequest, failed func() error) io.Reader {
	var complete bool
	return &ChunkReader{Next: func() ([]byte, bool, error) {
		for receive() {
			if complete {
				return nil, false, execenv.ErrInvalid
			}
			switch m := msg().GetPart().(type) {
			case *execenvv1.ImportRequest_Chunk:
				return m.Chunk, true, nil
			case *execenvv1.ImportRequest_Complete:
				if !m.Complete {
					return nil, false, execenv.ErrInvalid
				}
				complete = true
			default:
				return nil, false, execenv.ErrInvalid
			}
		}
		if err := failed(); err != nil {
			return nil, false, err
		}
		if !complete {
			return nil, false, execenv.ErrInvalid
		}
		return nil, false, nil
	}}
}

func SendImport(body io.Reader, send func(*execenvv1.ImportRequest) error) error {
	if body == nil {
		return execenv.ErrInvalid
	}
	if err := Pump(body, func(b []byte) error {
		return send(&execenvv1.ImportRequest{Part: &execenvv1.ImportRequest_Chunk{Chunk: b}})
	}); err != nil {
		return err
	}
	return send(&execenvv1.ImportRequest{Part: &execenvv1.ImportRequest_Complete{Complete: true}})
}

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
