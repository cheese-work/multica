<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
import { docsUrl } from "../../common/docs-url";
||||||| 2ae2f08005
import { docsLocalePrefix } from "../../common/docs-locale";
=======
import {
  docsLocalePrefix,
  isTraditionalChinese,
} from "../../common/docs-locale";
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d

export function daemonRuntimesDocsHref(language?: string): string {
  return docsUrl("/daemon-runtimes", language);
}

export function customRuntimeDocsHref(language?: string): string {
  const base = daemonRuntimesDocsHref(language);
  if (isTraditionalChinese(language)) {
    return `${base}#${encodeURIComponent("自定義執行時配置")}`;
  }
  if (language?.startsWith("zh")) {
    return `${base}#${encodeURIComponent("自定义运行时配置")}`;
  }
  if (language?.startsWith("ja")) {
    return `${base}#${encodeURIComponent("カスタムランタイムプロファイル")}`;
  }
  if (language?.startsWith("ko")) {
    return `${base}#${encodeURIComponent("사용자-지정-런타임-프로필")}`;
  }
  if (language?.startsWith("fr")) {
    return `${base}#${encodeURIComponent("profils-de-runtime-personnalisés")}`;
  }
  return `${base}#custom-runtime-profiles`;
}
