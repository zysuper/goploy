// Copyright 2022 The Goploy Authors. All rights reserved.
// Use of this source code is governed by a GPLv3-style
// license that can be found in the LICENSE file.

package transmitter

import (
	model2 "github.com/zhenorzz/goploy/internal/model"
)

type Transmitter interface {
	String() string
	Exec() (string, error)
}

func New(project model2.Project, server model2.ProjectServer) Transmitter {
	if project.TransferType == "sftp" {
		return sftpTransmitter{project, server}
	} else if project.TransferType == "custom" {
		return customTransmitter{project, server}
	} else {
		return rsyncTransmitter{project, server}
	}
}
