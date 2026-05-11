package _115_open_transcode

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	SourcePath string `json:"source_path" required:"true" type:"text" help:"Mount path of existing 115 Open storage, e.g. /115"`
}

var config = driver.Config{
	Name:        "115 Open Transcode",
	LocalSort:   true,
	NoCache:     true,
	NoUpload:    true,
	DefaultRoot: "/",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Open115Transcode{}
	})
}
