import { isTransforming, toImpl } from "./graph.js";
const cache = /*#__PURE__*/new WeakMap();
/** @internal */
export const getNodeIndex = (csr, nodeId) => csr.indexByNodeId === undefined ? Number.isInteger(nodeId) && nodeId >= 0 && nodeId < csr.nodeIds.length ? nodeId : undefined : csr.indexByNodeId.get(nodeId);
/** @internal */
const materializeEdges = csr => {
  if (csr.edgesByIndex !== undefined) {
    return;
  }
  const graphEdges = toImpl(csr.graph).edges;
  const edgeIds = new Array(graphEdges.size);
  const edges = new Array(graphEdges.size);
  let index = 0;
  for (const [edgeId, edge] of graphEdges) {
    edgeIds[index] = edgeId;
    edges[index++] = edge;
  }
  csr.edgeIdsByIndex = edgeIds;
  csr.edgesByIndex = edges;
};
/** @internal */
export const getEdges = csr => {
  materializeEdges(csr);
  return csr.edgesByIndex;
};
/** @internal */
export const getEdgeIds = csr => {
  materializeEdges(csr);
  return csr.edgeIdsByIndex;
};
const makeAdjacency = (csr, incoming) => {
  const impl = toImpl(csr.graph);
  const adjacencyMap = incoming ? impl.reverseAdjacency : impl.adjacency;
  // The first pass allocates exact row ranges; the second preserves canonical adjacency order within each row.
  const rowOffsets = new Uint32Array(csr.nodeIds.length + 1);
  for (let i = 0; i < csr.nodeIds.length; i++) {
    rowOffsets[i + 1] = rowOffsets[i] + (adjacencyMap.get(csr.nodeIds[i])?.length ?? 0);
  }
  const columnIndices = new Uint32Array(rowOffsets[csr.nodeIds.length]);
  for (let i = 0; i < csr.nodeIds.length; i++) {
    const nodeId = csr.nodeIds[i];
    const adjacency = adjacencyMap.get(nodeId);
    if (adjacency === undefined) {
      continue;
    }
    let offset = rowOffsets[i];
    for (let j = 0; j < adjacency.length; j++) {
      const edge = impl.edges.get(adjacency[j]);
      const neighbor = csr.type === "undirected" ? edge.source === nodeId ? edge.target : edge.source : incoming ? edge.source : edge.target;
      columnIndices[offset++] = csr.indexByNodeId === undefined ? neighbor : csr.indexByNodeId.get(neighbor);
    }
  }
  return {
    rowOffsets,
    columnIndices
  };
};
/** @internal */
export const getOutgoing = csr => {
  return csr.outgoingCsr ?? (csr.outgoingCsr = makeAdjacency(csr, false));
};
/** @internal */
export const getIncoming = csr => {
  return csr.incomingCsr ?? (csr.incomingCsr = makeAdjacency(csr, true));
};
/** @internal */
export const getOutgoingWithEdges = csr => {
  if (csr.outgoingEdgeCsr !== undefined) {
    return csr.outgoingEdgeCsr;
  }
  const impl = toImpl(csr.graph);
  if (csr.indexByEdgeId === undefined) {
    // Avoid a map lookup per adjacency entry when edge ids already match insertion-order positions.
    let compactEdgeIds = true;
    let position = 0;
    for (const edgeId of impl.edges.keys()) {
      if (edgeId !== position++) {
        compactEdgeIds = false;
        break;
      }
    }
    if (compactEdgeIds) {
      csr.indexByEdgeId = null;
    } else {
      csr.indexByEdgeId = new Map();
      position = 0;
      for (const edgeId of impl.edges.keys()) {
        csr.indexByEdgeId.set(edgeId, position++);
      }
    }
  }
  const outgoing = getOutgoing(csr);
  // Each slot identifies the edge responsible for the neighbor in the matching columnIndices slot.
  const edgeIndices = new Uint32Array(outgoing.columnIndices.length);
  let position = 0;
  for (const nodeId of csr.nodeIds) {
    const adjacency = impl.adjacency.get(nodeId);
    if (adjacency === undefined) {
      continue;
    }
    for (const edgeId of adjacency) {
      edgeIndices[position++] = csr.indexByEdgeId === null ? edgeId : csr.indexByEdgeId.get(edgeId);
    }
  }
  return csr.outgoingEdgeCsr = {
    ...outgoing,
    edgeIndices
  };
};
/** @internal */
export const getAdjacencies = (csr, direction) => {
  if (csr.type === "directed" && direction === "incoming") {
    return {
      primary: getIncoming(csr),
      secondary: undefined
    };
  }
  if (csr.type === "directed" && direction === "undirected") {
    // A directed graph needs both matrices to ignore direction; undirected rows already contain both endpoints.
    return {
      primary: getOutgoing(csr),
      secondary: getIncoming(csr)
    };
  }
  return {
    primary: getOutgoing(csr),
    secondary: undefined
  };
};
/** @internal */
export const getEdgeEndpoints = csr => {
  if (csr.compactEdgeEndpoints !== undefined) {
    return csr.compactEdgeEndpoints;
  }
  const edges = getEdges(csr);
  const sources = new Uint32Array(edges.length);
  const targets = new Uint32Array(edges.length);
  for (let i = 0; i < edges.length; i++) {
    sources[i] = csr.indexByNodeId === undefined ? edges[i].source : csr.indexByNodeId.get(edges[i].source);
    targets[i] = csr.indexByNodeId === undefined ? edges[i].target : csr.indexByNodeId.get(edges[i].target);
  }
  return csr.compactEdgeEndpoints = {
    sources,
    targets
  };
};
/** @internal */
export const peek = graph => cache.get(graph);
/** @internal */
export const invalidate = graph => {
  // Existing iterators retain their Csr object; only subsequent lookups rebuild from the mutated graph.
  cache.delete(graph);
};
/** @internal */
export const get = graph => {
  const cacheEnabled = !isTransforming(graph);
  if (cacheEnabled) {
    const cached = cache.get(graph);
    if (cached !== undefined) {
      return cached;
    }
  }
  // Node ids and data are captured together so an iterator never consults mutable maps after it starts.
  const impl = toImpl(graph);
  const nodeIds = new Array(impl.nodes.size);
  const nodeData = new Array(impl.nodes.size);
  let compactNodeIds = true;
  let nodePosition = 0;
  for (const [nodeId, data] of impl.nodes) {
    nodeIds[nodePosition] = nodeId;
    nodeData[nodePosition] = data;
    if (nodeId !== nodePosition) {
      compactNodeIds = false;
    }
    nodePosition++;
  }
  const indexByNodeId = compactNodeIds ? undefined : new Map();
  if (indexByNodeId !== undefined) {
    for (let i = 0; i < nodeIds.length; i++) {
      indexByNodeId.set(nodeIds[i], i);
    }
  }
  const result = {
    type: impl.type,
    graph,
    nodeIds,
    nodeData,
    indexByNodeId,
    outgoingCsr: undefined,
    incomingCsr: undefined,
    outgoingEdgeCsr: undefined,
    edgesByIndex: undefined,
    edgeIdsByIndex: undefined,
    compactEdgeEndpoints: undefined,
    indexByEdgeId: undefined
  };
  if (cacheEnabled) {
    cache.set(graph, result);
  }
  return result;
};
//# sourceMappingURL=graphCsr.js.map