package radar

import _ "embed"

// drsModelArtifactJSON is the trained DRS model compiled into the binary, so a
// release archive is self-contained and the model version is pinned by the
// same RADAR_VERSION the consuming workflow already pins. The committed file is
// a placeholder until the training pipeline produces a real artifact;
// NewDRSModelScorer refuses it.
//
//go:embed drs_model.json
var drsModelArtifactJSON []byte
