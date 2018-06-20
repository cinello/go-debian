/* {{{ Copyright (c) Paul R. Tagliamonte <paultag@debian.org>, 2015
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in
 * all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
 * THE SOFTWARE. }}} */

package control

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/gandalfmagic/go-debian/dependency"
        "github.com/gandalfmagic/go-debian/internal"
	"github.com/gandalfmagic/go-debian/version"

	"pault.ag/go/topsort"
)

// A DSC is the encapsulation of a Debian .dsc control file. This contains
// information about the source package, and is general handy.
//
// The Debian source control file is generated by dpkg-source when it builds
// the source archive, from other files in the source package.
// When unpacking, it is checked against the files and directories in the
// other parts of the source package.
type DSC struct {
	Paragraph

	Filename string

	Format           string
	Source           string
	Binaries         []string          `control:"Binary" delim:","`
	Architectures    []dependency.Arch `control:"Architecture"`
	Version          version.Version
	Origin           string
	Maintainer       string
	Uploaders        []string
	Homepage         string
	StandardsVersion string `control:"Standards-Version"`

	BuildDepends      dependency.Dependency `control:"Build-Depends"`
	BuildDependsArch  dependency.Dependency `control:"Build-Depends-Arch"`
	BuildDependsIndep dependency.Dependency `control:"Build-Depends-Indep"`

	ChecksumsSha1   []SHA1FileHash   `control:"Checksums-Sha1" delim:"\n" strip:"\n\r\t "`
	ChecksumsSha256 []SHA256FileHash `control:"Checksums-Sha256" delim:"\n" strip:"\n\r\t "`
	Files           []MD5FileHash    `control:"Files" delim:"\n" strip:"\n\r\t "`

	/*
		TODO:
			Package-List
	*/
}

// Given a bunch of DSC objects, sort the packages topologically by
// build order by looking at the relationship between the Build-Depends
// field.
func OrderDSCForBuild(dscs []DSC, arch dependency.Arch) ([]DSC, error) {
	sourceMapping := map[string]string{}
	network := topsort.NewNetwork()
	ret := []DSC{}

	/*
	 * - Create binary -> source mapping.
	 * - Error if two sources provide the same binary
	 * - Create a node for each source
	 * - Create an edge from the source -> source
	 * - return sorted list of dsc files
	 */

	for _, dsc := range dscs {
		for _, binary := range dsc.Binaries {
			sourceMapping[binary] = dsc.Source
		}
		network.AddNode(dsc.Source, dsc)
	}

	for _, dsc := range dscs {
		concreteBuildDepends := []dependency.Possibility{}
		concreteBuildDepends = append(concreteBuildDepends, dsc.BuildDepends.GetPossibilities(arch)...)
		concreteBuildDepends = append(concreteBuildDepends, dsc.BuildDependsArch.GetPossibilities(arch)...)
		concreteBuildDepends = append(concreteBuildDepends, dsc.BuildDependsIndep.GetPossibilities(arch)...)
		for _, relation := range concreteBuildDepends {
			if val, ok := sourceMapping[relation.Name]; ok {
				err := network.AddEdge(val, dsc.Source)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	nodes, err := network.Sort()
	if err != nil {
		return nil, err
	}

	for _, node := range nodes {
		ret = append(ret, node.Value.(DSC))
	}

	return ret, nil
}

// Given a path on the filesystem, Parse the file off the disk and return
// a pointer to a brand new DSC struct, unless error is set to a value
// other than nil.
func ParseDscFile(path string) (ret *DSC, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ret, err = ParseDsc(bufio.NewReader(f), path)
	if err != nil {
		return nil, err
	}
	return ret, nil
}

// Given a bufio.Reader, consume the Reader, and return a DSC object
// for use.
func ParseDsc(reader *bufio.Reader, path string) (*DSC, error) {
	ret := DSC{Filename: path}
	err := Unmarshal(&ret, reader)
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

// Check to see if this .dsc contains any arch:all binary packages along
// with any arch dependent packages.
func (d *DSC) HasArchAll() bool {
	for _, arch := range d.Architectures {
		if arch.CPU == "all" && arch.OS == "all" && arch.ABI == "all" {
			return true
		}
	}
	return false
}

// Return a list of all entities that are responsible for the package's
// well being. The 0th element is always the package's Maintainer,
// with any Uploaders following.
func (d *DSC) Maintainers() []string {
	return append([]string{d.Maintainer}, d.Uploaders...)
}

// Return a list of MD5FileHash entries from the `dsc.Files`
// entry, with the exception that each `Filename` will be joined to the root
// directory of the DSC file.
func (d *DSC) AbsFiles() []MD5FileHash {
	ret := []MD5FileHash{}

	baseDir := filepath.Dir(d.Filename)
	for _, hash := range d.Files {
		hash.Filename = path.Join(baseDir, hash.Filename)
		ret = append(ret, hash)
	}

	return ret
}

// Copy the .dsc file and all referenced files to the directory
// listed by the dest argument. This function will error out if the dest
// argument is not a directory, or if there is an IO operation in transfer.
//
// This function will always move .dsc last, making it suitable to
// be used to move something into an incoming directory with an inotify
// hook. This will also mutate DSC.Filename to match the new location.
func (d *DSC) Copy(dest string) error {
	if file, err := os.Stat(dest); err == nil && !file.IsDir() {
		return fmt.Errorf("Attempting to move .dsc to a non-directory")
	}

	for _, file := range d.AbsFiles() {
		dirname := filepath.Base(file.Filename)
		err := internal.Copy(file.Filename, dest+"/"+dirname)
		if err != nil {
			return err
		}
	}

	dirname := filepath.Base(d.Filename)
	err := internal.Copy(d.Filename, dest+"/"+dirname)
	d.Filename = dest + "/" + dirname
	return err
}

// Move the .dsc file and all referenced files to the directory
// listed by the dest argument. This function will error out if the dest
// argument is not a directory, or if there is an IO operation in transfer.
//
// This function will always move .dsc last, making it suitable to
// be used to move something into an incoming directory with an inotify
// hook. This will also mutate DSC.Filename to match the new location.
func (d *DSC) Move(dest string) error {
	if file, err := os.Stat(dest); err == nil && !file.IsDir() {
		return fmt.Errorf("Attempting to move .dsc to a non-directory")
	}

	for _, file := range d.AbsFiles() {
		dirname := filepath.Base(file.Filename)
		err := os.Rename(file.Filename, dest+"/"+dirname)
		if err != nil {
			return err
		}
	}

	dirname := filepath.Base(d.Filename)
	err := os.Rename(d.Filename, dest+"/"+dirname)
	d.Filename = dest + "/" + dirname
	return err
}

// Remove the .dsc file and any associated files. This function will
// always remove the .dsc last, in the event there are filesystem i/o errors
// on removing associated files.
func (d *DSC) Remove() error {
	for _, file := range d.AbsFiles() {
		err := os.Remove(file.Filename)
		if err != nil {
			return err
		}
	}
	return os.Remove(d.Filename)
}

// vim: foldmethod=marker
