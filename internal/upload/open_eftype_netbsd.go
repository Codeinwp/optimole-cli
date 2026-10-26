package upload

import (
	"errors"
	"syscall"
)

func isEFTYPE(err error) bool { return errors.Is(err, syscall.EFTYPE) }
