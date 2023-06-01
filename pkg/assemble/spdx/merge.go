// Copyright 2023 Interlynk.io
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package spdx

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
	"github.com/google/uuid"
	"github.com/interlynk-io/sbomasm/pkg/logger"
	"github.com/samber/lo"
	"github.com/spdx/tools-golang/spdx"
	"github.com/spdx/tools-golang/spdx/v2/common"
	"github.com/spdx/tools-golang/spdx/v2/v2_3"
	"sigs.k8s.io/release-utils/version"
)

type merge struct {
	settings      *MergeSettings
	out           *spdx.Document
	in            []*spdx.Document
	rootPackageID string
}

func newMerge(ms *MergeSettings) *merge {
	return &merge{
		settings:      ms,
		in:            []*spdx.Document{},
		out:           &spdx.Document{},
		rootPackageID: uuid.New().String(),
	}
}

func (m *merge) loadBoms() {
	for _, path := range m.settings.Input.Files {
		bom, err := loadBom(*m.settings.Ctx, path)
		if err != nil {
			panic(err) // TODO: return error instead of panic
		}
		m.in = append(m.in, bom)
	}
}

func (m *merge) initOutBom() {
	log := logger.FromContext(*m.settings.Ctx)
	m.out.SPDXVersion = spdx.Version
	m.out.DataLicense = spdx.DataLicense
	m.out.SPDXIdentifier = common.ElementID("DOCUMENT")
	m.out.DocumentName = m.settings.App.Name
	m.out.DocumentNamespace = composeNamespace(m.settings.App.Name)
	m.out.CreationInfo = &v2_3.CreationInfo{}
	m.out.CreationInfo.Created = utcNowTime()

	m.out.CreationInfo.CreatorComment = fmt.Sprintf("Generated by sbomasm-%s using %d sboms",
		version.GetVersionInfo().GitVersion,
		len(m.in))

	for _, author := range m.settings.App.Authors {
		c := common.Creator{}
		c.CreatorType = "Organization"

		if author.Name != "" || author.Email != "" {
			c.Creator = fmt.Sprintf("%s (%s)", author.Name, author.Email)
		}
		m.out.CreationInfo.Creators = append(m.out.CreationInfo.Creators, c)
	}

	//Add tool also as creator
	m.out.CreationInfo.Creators = append(m.out.CreationInfo.Creators, common.Creator{
		CreatorType: "Tool",
		Creator:     fmt.Sprintf("%s-%s", "sbomasm", version.GetVersionInfo().GitVersion),
	})

	lVersions := lo.Uniq(lo.Map(m.in, func(bom *spdx.Document, _ int) string {
		if bom.CreationInfo != nil && bom.CreationInfo.LicenseListVersion != "" {
			return bom.CreationInfo.LicenseListVersion
		}
		return ""
	}))

	finalLicVersion := "3.19"
	if len(lVersions) > 1 {
		vs := make([]*semver.Version, len(lVersions))
		for i, r := range lVersions {
			v, err := semver.NewVersion(r)
			if err != nil {
				panic(err) // TODO: return error instead of panic
			}
			vs[i] = v
		}

		sort.Sort(semver.Collection(vs))
		finalLicVersion = vs[0].String()
	} else if len(lVersions) == 1 && strings.Trim(lVersions[0], " ") != "" {
		finalLicVersion = lVersions[0]
	}
	log.Debugf("No of Licenses: %d:  Selected:%s", len(lVersions), finalLicVersion)
	m.out.CreationInfo.LicenseListVersion = finalLicVersion

	m.out.ExternalDocumentReferences = lo.FlatMap(m.in, func(bom *spdx.Document, _ int) []spdx.ExternalDocumentRef {
		return bom.ExternalDocumentReferences
	})

}

func (m *merge) setupPrimaryComp() *spdx.Package {
	p := &spdx.Package{}
	p.PackageName = m.settings.App.Name
	p.PackageVersion = m.settings.App.Version
	p.PackageSPDXIdentifier = common.ElementID(fmt.Sprintf("RootPackage-%s", m.rootPackageID))
	p.PackageDownloadLocation = "NOASSERTION"

	if m.settings.App.Supplier.Name != "" || m.settings.App.Supplier.Email != "" {
		p.PackageSupplier = &common.Supplier{}
		p.PackageSupplier.SupplierType = "Organization"
		p.PackageSupplier.Supplier = fmt.Sprintf("%s (%s)", m.settings.App.Supplier.Name, m.settings.App.Supplier.Email)
	} else {
		p.PackageSupplier = &common.Supplier{}
		p.PackageSupplier.SupplierType = "NOASSERTION"
		p.PackageSupplier.Supplier = "NOASSERTION"
	}

	p.FilesAnalyzed = false

	if len(m.settings.App.Checksums) > 0 {
		p.PackageChecksums = []common.Checksum{}
		for _, c := range m.settings.App.Checksums {
			if len(c.Value) == 0 {
				continue
			}
			p.PackageChecksums = append(p.PackageChecksums, common.Checksum{
				Algorithm: spdx_hash_algos[c.Algorithm],
				Value:     c.Value,
			})
		}
	}

	if m.settings.App.License.Id != "" {
		p.PackageLicenseConcluded = m.settings.App.License.Id
	}

	if m.settings.App.License.Expression != "" {
		p.PackageLicenseConcluded = m.settings.App.License.Expression
	}

	if m.settings.App.License.Expression != "" && m.settings.App.License.Id == "" {
		p.PackageLicenseDeclared = "NOASSERTION"
		p.PackageLicenseConcluded = "NOASSERTION"
	}

	if m.settings.App.Copyright != "" {
		p.PackageCopyrightText = m.settings.App.Copyright
	} else {
		p.PackageCopyrightText = "NOASSERTION"
	}

	p.PackageDescription = m.settings.App.Description
	p.PrimaryPackagePurpose = m.settings.App.PrimaryPurpose

	p.PackageExternalReferences = []*spdx.PackageExternalReference{}

	if m.settings.App.Purl != "" {
		purl := spdx.PackageExternalReference{
			Category: common.CategoryPackageManager,
			RefType:  common.TypePackageManagerPURL,
			Locator:  m.settings.App.Purl,
		}
		p.PackageExternalReferences = append(p.PackageExternalReferences, &purl)
	}

	if m.settings.App.CPE != "" {
		cpe := spdx.PackageExternalReference{
			Category: common.CategorySecurity,
			RefType:  common.TypeSecurityCPE23Type,
			Locator:  m.settings.App.CPE,
		}
		p.PackageExternalReferences = append(p.PackageExternalReferences, &cpe)
	}

	return p
}

func (m *merge) isDescribedPackage(pkg *spdx.Package, descRels []*spdx.Relationship) bool {
	if pkg == nil {
		return false
	}

	for _, rel := range descRels {
		if rel == nil {
			continue
		}
		if rel.RefB.ElementRefID == pkg.PackageSPDXIdentifier {
			return true
		}
	}

	return false
}

func (m *merge) hierarchicalMerge() error {
	log := logger.FromContext(*m.settings.Ctx)

	pc := m.setupPrimaryComp()

	log.Debugf("primary component id: %s", pc.PackageSPDXIdentifier)

	pkgs := []*spdx.Package{pc}
	deps := []*spdx.Relationship{}

	//Add relationship between document and primary package
	deps = append(deps, &spdx.Relationship{
		RefA:         common.MakeDocElementID("", "DOCUMENT"),
		RefB:         common.MakeDocElementID("", string(pc.PackageSPDXIdentifier)),
		Relationship: common.TypeRelationshipDescribe,
	})

	for _, doc := range m.in {
		log.Debugf("processing sbom %s with packages:%d, files:%d, deps:%d, Snips:%d OtherLics:%d, Annotations:%d, externaldocrefs:%d",
			fmt.Sprintf("%s-%s", doc.SPDXIdentifier, doc.DocumentName),
			len(doc.Packages), len(doc.Files), len(doc.Relationships),
			len(doc.Snippets), len(doc.OtherLicenses), len(doc.Annotations),
			len(doc.ExternalDocumentReferences))

		descRels := lo.Filter(doc.Relationships, func(rel *spdx.Relationship, _ int) bool {
			if rel == nil {
				return false
			}
			return rel.Relationship == common.TypeRelationshipDescribe
		})

		for _, pkg := range doc.Packages {
			isDescPkg := m.isDescribedPackage(pkg, descRels)

			cPkg, err := cloneComp(pkg)
			if err != nil {
				log.Warnf("Failed to clone component: %s : %s", pkg.PackageSPDXIdentifier, pkg.PackageName)
				continue
			}

			if isDescPkg {
				//Change the SPDX Identifier to the package specified
				cPkg.PackageSPDXIdentifier = common.ElementID(fmt.Sprintf("Package-%s", uuid.New().String()))

				deps = append(deps, &spdx.Relationship{
					RefA:         common.MakeDocElementID("", string(pc.PackageSPDXIdentifier)),
					RefB:         common.MakeDocElementID("", string(cPkg.PackageSPDXIdentifier)),
					Relationship: common.TypeRelationshipContains,
				})

				//Update the relationships to point to the new package id
				lo.ForEach(doc.Relationships, func(rel *spdx.Relationship, _ int) {
					if rel == nil {
						return
					}
					if rel.RefA.ElementRefID == pkg.PackageSPDXIdentifier {
						rel.RefA.ElementRefID = cPkg.PackageSPDXIdentifier
					}
					if rel.RefB.ElementRefID == pkg.PackageSPDXIdentifier {
						rel.RefB.ElementRefID = cPkg.PackageSPDXIdentifier
					}
				})
			}
			pkgs = append(pkgs, cPkg)
		}
	}

	deps = append(deps, lo.FlatMap(m.in, func(doc *spdx.Document, _ int) []*spdx.Relationship {
		return lo.Filter(doc.Relationships, func(rel *spdx.Relationship, _ int) bool {
			if rel == nil {
				return false
			}
			return rel.Relationship != common.TypeRelationshipDescribe
		})
	})...)

	files := lo.Flatten(lo.Map(m.in, func(pkg *spdx.Document, _ int) []*spdx.File {
		return pkg.Files
	}))

	otherLics := lo.FlatMap(m.in, func(doc *spdx.Document, _ int) []*spdx.OtherLicense {
		return doc.OtherLicenses
	})

	m.out.Packages = pkgs
	m.out.Files = files
	m.out.Relationships = deps
	m.out.OtherLicenses = otherLics

	return m.writeSBOM()
}

func (m *merge) flatMerge() error {
	return fmt.Errorf("spdx flat merge not implemented")
}

func (m *merge) writeSBOM() error {
	log := logger.FromContext(*m.settings.Ctx)
	var f io.Writer
	outName := "stdout"

	if m.settings.Output.File == "" {
		f = os.Stdout
	} else {
		var err error
		outName = m.settings.Output.File
		f, err = os.Create(m.settings.Output.File)
		if err != nil {
			return err
		}
	}

	buf, err := json.MarshalIndent(m.out, "", " ")
	if err != nil {
		return err
	}

	_, err = f.Write(buf)
	if err != nil {
		return err
	}

	log.Debugf("wrote sbom %d bytes to %s with packages:%d, files:%d, deps:%d, snips:%d otherLics:%d, annotations:%d, externaldocRefs:%d",
		len(buf), outName,
		len(m.out.Packages), len(m.out.Files), len(m.out.Relationships),
		len(m.out.Snippets), len(m.out.OtherLicenses), len(m.out.Annotations),
		len(m.out.ExternalDocumentReferences))

	return nil
}