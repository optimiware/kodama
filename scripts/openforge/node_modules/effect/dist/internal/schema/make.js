import * as Pipeable from "../../Pipeable.js";
import * as SchemaAST from "../../SchemaAST.js";
import * as SchemaParser from "../../SchemaParser.js";
/** @internal */
export const TypeId = "~effect/Schema/Schema";
const RebuildOptions = /*#__PURE__*/Symbol();
const SchemaProto = {
  [TypeId]: TypeId,
  get make() {
    const value = SchemaParser.make(this);
    Object.defineProperty(this, "make", {
      value,
      enumerable: true
    });
    return value;
  },
  get makeEffect() {
    const value = SchemaParser.makeEffect(this);
    Object.defineProperty(this, "makeEffect", {
      value,
      enumerable: true
    });
    return value;
  },
  get makeOption() {
    const value = SchemaParser.makeOption(this);
    Object.defineProperty(this, "makeOption", {
      value,
      enumerable: true
    });
    return value;
  },
  pipe() {
    return Pipeable.pipeArguments(this, arguments);
  },
  annotate(annotations) {
    return this.rebuild(SchemaAST.annotate(this.ast, annotations));
  },
  annotateKey(annotations) {
    return this.rebuild(SchemaAST.annotateKey(this.ast, annotations));
  },
  check(...checks) {
    return this.rebuild(SchemaAST.appendChecks(this.ast, checks));
  },
  rebuild(ast) {
    return make(ast, this[RebuildOptions]);
  }
};
/** @internal */
export function make(ast, options) {
  function Schema() {}
  const self = Object.setPrototypeOf(Schema, SchemaProto);
  if (options && (Object.hasOwn(options, "name") || Object.hasOwn(options, "length") || Object.hasOwn(options, "__proto__"))) {
    Object.defineProperties(self, Object.getOwnPropertyDescriptors({
      ...options
    }));
  } else {
    Object.assign(self, options);
  }
  self[RebuildOptions] = options;
  self.ast = ast;
  return self;
}
//# sourceMappingURL=make.js.map