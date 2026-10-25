import * as Schema from "../../Schema.js";
import * as InternalStructuredOutput from "./structured-output.js";
const makeDefaultCodecTransformer = toJsonSchemaDocument => {
  return codec => {
    const document = InternalStructuredOutput.resolveTopLevelReference(toJsonSchemaDocument(codec));
    const jsonSchema = {
      ...document.schema
    };
    if (Object.keys(document.definitions).length > 0) {
      jsonSchema.$defs = document.definitions;
    }
    return {
      codec,
      jsonSchema
    };
  };
};
/**
 * @internal
 */
export const defaultCodecTransformer = /*#__PURE__*/makeDefaultCodecTransformer(Schema.toJsonSchemaDocument);
//# sourceMappingURL=codec-transformer.js.map