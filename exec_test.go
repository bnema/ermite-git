// SPDX-License-Identifier: MIT

package git

import "os/exec"

func execCommand(dir, gitDir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), "GIT_DIR="+gitDir)
	return cmd
}

func (f *fx) gitErr(args ...string) (string, error) {
	out, err := execCommand(f.root(), f.dir, args...).Output()
	return string(out), err
}
