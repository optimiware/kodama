import * as Equal from "../Equal.js";
import * as Hash from "../Hash.js";
import { NodeInspectSymbol } from "../Inspectable.js";
import * as Option from "../Option.js";
import { pipeArguments } from "../Pipeable.js";
import { hasProperty } from "../Predicate.js";
/** @internal */
export const TypeId = "~effect/Graph";
/** @internal */
export const isGraph = u => hasProperty(u, TypeId);
/** @internal */
export const toImpl = graph => graph;
/** @internal */
export const isTransforming = graph => toImpl(graph).transforming;
/** @internal */
export const withTransformation = (graph, evaluate) => {
  const impl = toImpl(graph);
  const transforming = impl.transforming;
  impl.transforming = true;
  try {
    return evaluate();
  } finally {
    impl.transforming = transforming;
  }
};
const edgeEquals = (type, self, that) => (type === "directed" ? self.source === that.source && self.target === that.target : self.source === that.source && self.target === that.target || self.source === that.target && self.target === that.source) && Equal.equals(self.data, that.data);
const edgeHash = (type, edge) => type === "directed" ? Hash.hash(edge) : Hash.optimize(Hash.combine(Hash.hash(edge.data), endpointsHash(edge.source, edge.target)));
/**
 * Addition is commutative and does not cancel self-loops.
 *
 * @internal
 */
export const endpointsHash = (source, target) => Hash.combine(0, Hash.hash(source)) + Hash.combine(0, Hash.hash(target));
const graphSeed = /*#__PURE__*/Hash.string("Graph");
const ProtoGraph = {
  [TypeId]: {
    _N: _ => _,
    _E: _ => _
  },
  [Symbol.iterator]() {
    return this.nodes[Symbol.iterator]();
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  },
  [Equal.symbol](that) {
    if (hasProperty(that, TypeId)) {
      const thatImpl = toImpl(that);
      if (this.nodes.size !== thatImpl.nodes.size || this.edges.size !== thatImpl.edges.size || this.type !== thatImpl.type) {
        return false;
      }
      for (const [nodeIndex, nodeData] of this.nodes) {
        if (!thatImpl.nodes.has(nodeIndex) || !Equal.equals(nodeData, thatImpl.nodes.get(nodeIndex))) {
          return false;
        }
      }
      for (const [edgeIndex, edgeData] of this.edges) {
        const otherEdge = thatImpl.edges.get(edgeIndex);
        if (otherEdge === undefined || !edgeEquals(this.type, edgeData, otherEdge)) {
          return false;
        }
      }
      return true;
    }
    return false;
  },
  [Hash.symbol]() {
    let hash = graphSeed;
    hash = hash ^ Hash.string(this.type);
    hash = hash ^ Hash.number(this.nodes.size);
    hash = hash ^ Hash.number(this.edges.size);
    for (const [nodeIndex, nodeData] of this.nodes) {
      hash ^= Hash.combine(Hash.hash(nodeIndex), Hash.hash(nodeData));
    }
    for (const [edgeIndex, edgeData] of this.edges) {
      hash ^= Hash.combine(Hash.hash(edgeIndex), edgeHash(this.type, edgeData));
    }
    return Hash.optimize(hash);
  },
  toJSON() {
    return {
      _id: "Graph",
      nodeCount: this.nodes.size,
      edgeCount: this.edges.size,
      type: this.type
    };
  },
  toString() {
    return `Graph(${this.type}, ${this.nodes.size}, ${this.edges.size})`;
  },
  pipe() {
    return pipeArguments(this, arguments);
  }
};
/** @internal */
export const make = (type, mutable) => {
  const graph = Object.create(ProtoGraph);
  graph.type = type;
  graph.mutable = mutable;
  graph.transforming = false;
  graph.nodes = new Map();
  graph.edges = new Map();
  graph.adjacency = new Map();
  graph.reverseAdjacency = new Map();
  graph.nextNodeIndex = 0;
  graph.nextEdgeIndex = 0;
  graph.acyclic = Option.some(true);
  return graph;
};
const cloneAdjacency = adjacency => {
  const cloned = new Map();
  for (const [nodeIndex, edges] of adjacency) {
    cloned.set(nodeIndex, [...edges]);
  }
  return cloned;
};
/** @internal */
export const clone = (source, mutable) => {
  const graph = make(source.type, mutable);
  graph.nodes = new Map(source.nodes);
  graph.edges = new Map(source.edges);
  graph.adjacency = cloneAdjacency(source.adjacency);
  graph.reverseAdjacency = cloneAdjacency(source.reverseAdjacency);
  graph.nextNodeIndex = source.nextNodeIndex;
  graph.nextEdgeIndex = source.nextEdgeIndex;
  graph.acyclic = source.acyclic;
  return graph;
};
/** @internal */
export const finalize = source => {
  const graph = Object.create(ProtoGraph);
  graph.type = source.type;
  graph.mutable = false;
  graph.transforming = false;
  graph.nodes = source.nodes;
  graph.edges = source.edges;
  graph.adjacency = source.adjacency;
  graph.reverseAdjacency = source.reverseAdjacency;
  graph.nextNodeIndex = source.nextNodeIndex;
  graph.nextEdgeIndex = source.nextEdgeIndex;
  graph.acyclic = source.acyclic;
  return graph;
};
/** @internal */
export const snapshot = graph => {
  const impl = toImpl(graph);
  return {
    type: graph.type,
    nodes: Array.from(impl.nodes, ([index, data]) => ({
      index,
      data
    })).sort((a, b) => a.index - b.index),
    edges: Array.from(impl.edges, ([index, edge]) => ({
      index,
      source: edge.source,
      target: edge.target,
      data: edge.data
    })).sort((a, b) => a.index - b.index)
  };
};
/** @internal */
export const hydrate = snapshot => {
  const graph = make(snapshot.type, false);
  for (const node of snapshot.nodes) {
    graph.nodes.set(node.index, node.data);
    graph.adjacency.set(node.index, []);
    graph.reverseAdjacency.set(node.index, []);
  }
  for (const edge of snapshot.edges) {
    graph.edges.set(edge.index, {
      source: edge.source,
      target: edge.target,
      data: edge.data
    });
    graph.adjacency.get(edge.source).push(edge.index);
    graph.reverseAdjacency.get(edge.target).push(edge.index);
    if (snapshot.type === "undirected") {
      graph.adjacency.get(edge.target).push(edge.index);
      graph.reverseAdjacency.get(edge.source).push(edge.index);
    }
  }
  graph.nextNodeIndex = snapshot.nodes.length === 0 ? 0 : snapshot.nodes[snapshot.nodes.length - 1].index + 1;
  graph.nextEdgeIndex = snapshot.edges.length === 0 ? 0 : snapshot.edges[snapshot.edges.length - 1].index + 1;
  graph.acyclic = Option.none();
  return graph;
};
//# sourceMappingURL=graph.js.map