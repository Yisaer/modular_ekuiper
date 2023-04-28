#  Copyright 2022 EMQ Technologies Co., Ltd.
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.

from ekuiper import plugin, PluginConfig

from chebyshev import chebyshevIns
from butterworth import butterworthIns
from dataPreprocess import cleanDropIns, cleanNormalIns, cleanSortIns, dimensionReductionIns
from dataSmooth import datasmoothIns
from fftpower import fftpowerIns
from peakvallydet import peakVallydetIns
from statistic import listAverageIns, listMaxIns, listMinIns, listAmpIns, listRmsIns, \
    listVarianceIns, listStandardDeviationIns

if __name__ == '__main__':
    c = PluginConfig("crrcfuncs", {}, {},
                     {"chebyshev": lambda: chebyshevIns, "butterworth": lambda: butterworthIns,
                      "datasmooth": lambda: datasmoothIns, "fftpower": lambda: fftpowerIns,
                      "peakvallydet": lambda: peakVallydetIns,
                      "listaverage": lambda: listAverageIns, "listmin": lambda: listMinIns,
                      "listmax": lambda: listMaxIns, "listamp": lambda: listAmpIns, "listrms": lambda: listRmsIns,
                      "listvariance": lambda: listVarianceIns,
                      "liststandarddeviation": lambda: listStandardDeviationIns, "cleandrop": lambda: cleanDropIns,
                      "cleannormal": lambda: cleanNormalIns,
                      "cleansort": lambda: cleanSortIns, "dimensionreduction": lambda: dimensionReductionIns})
    plugin.start(c)
