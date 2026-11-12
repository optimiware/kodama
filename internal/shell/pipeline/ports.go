package pipeline

// This package declares no effectful pipeline port.
//
// Definition reads and writes commit through the operation registry
// (pipeline.apply, pipeline.read, pipeline.update, pipeline.delete) and the
// immutable admission/start commit is pinned by task.StartUnit, which carries
// the admitted Snapshot. Registry resolution and YAML decoding sit behind
// adapter seams; a pipeline repository split here would separate the snapshot
// from the execution it admits.
