import { Layer } from "effect";
import { LayerNode } from "@opencode/util/effect/layer-node";
import { Instance } from "./instance.js";
import { LocationServiceMap } from "./location-service-map.js";
export { LocationServiceMap } from "./location-service-map.js";
export type LocationServices = Instance.Services;
export declare function buildLocationServiceMap(replacements?: LayerNode.Replacements): Layer.Layer<LocationServiceMap.Service>;
